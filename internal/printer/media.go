package printer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func WriteJSON(path string, data any) error {
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(encoded, '\n'))
}

// WriteFile is atomic and refuses existing files, including symlinks.
func WriteFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".elegooctl-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Hard-linking gives atomic no-replace behavior (same-directory temp file).
	return os.Link(file.Name(), path)
}

func (c *Client) Snapshot(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("output already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := c.do(ctx, "GET", c.endpoint("http", c.CameraPort, "/video"), "", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("camera HTTP %d", response.StatusCode)
	}
	media, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	var reader io.Reader = response.Body
	if media == "multipart/x-mixed-replace" {
		if params["boundary"] == "" {
			return errors.New("camera response has no multipart boundary")
		}
		buffered := bufio.NewReader(response.Body)
		first, err := buffered.ReadSlice('\n')
		if err != nil {
			return err
		}
		boundary := params["boundary"]
		// Some camera firmware includes the delimiter's '--' in the boundary
		// parameter but omits the extra '--' required on the wire. Detect the
		// actual first delimiter rather than breaking correctly formed streams.
		if strings.HasPrefix(boundary, "--") && strings.TrimSpace(string(first)) == boundary {
			boundary = strings.TrimPrefix(boundary, "--")
		}
		part, err := multipart.NewReader(io.MultiReader(bytes.NewReader(first), buffered), boundary).NextPart()
		if err != nil {
			return err
		}
		defer part.Close()
		reader = part
	} else if media != "image/jpeg" {
		return fmt.Errorf("unsupported camera content type %q", media)
	}
	const limit = 10 << 20
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("camera frame exceeds 10 MiB")
	}
	if _, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("invalid camera JPEG: %w", err)
	}
	return WriteFile(path, data)
}

type Recording struct {
	Filename       string   `json:"filename"`
	TaskID         string   `json:"task_id"`
	Segments       []string `json:"segments"`
	CameraFailures int      `json:"camera_failures"`
	Finished       bool     `json:"finished"`
	Canceled       bool     `json:"canceled"`
	Error          string   `json:"error,omitempty"`
}

// Record saves separate MP4 segments on reconnect, plus status JSONL and a manifest.
// ffmpeg is an optional runtime dependency of this command only.
func (c *Client) Record(ctx context.Context, first Status, directory string, retries int, log io.Writer) (resultErr error) {
	if retries < 0 {
		return errors.New("camera retries must be nonnegative")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("record requires ffmpeg on PATH")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return fmt.Errorf("record output must be a new directory: %w", err)
	}
	trace, err := os.OpenFile(filepath.Join(directory, "status.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer trace.Close()
	encoder := json.NewEncoder(trace)
	manifest := Recording{Filename: Filename(first.Print.Filename), TaskID: first.Print.TaskID}
	var camera *exec.Cmd
	var stopped chan error
	attempt := 0
	startCamera := func() error {
		attempt++
		name := fmt.Sprintf("video-%03d.mp4", attempt)
		camera = exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
			"-rw_timeout", "10000000", "-use_wallclock_as_timestamps", "1", "-probesize", "32", "-analyzeduration", "0",
			"-f", "mpjpeg", "-i", c.endpoint("http", c.CameraPort, "/video"), "-vf", "fps=5",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-movflags", "+faststart", filepath.Join(directory, name))
		camera.Stderr = log
		if err := camera.Start(); err != nil {
			return err
		}
		manifest.Segments = append(manifest.Segments, name)
		stopped = make(chan error, 1)
		cmd, ch := camera, stopped
		go func() { ch <- cmd.Wait() }()
		return nil
	}
	defer func() {
		if camera != nil && camera.Process != nil {
			camera.Process.Signal(os.Interrupt)
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				camera.Process.Kill()
				<-stopped
			}
		}
		if resultErr != nil {
			manifest.Error = resultErr.Error()
		}
		if err := WriteJSON(filepath.Join(directory, "recording.json"), manifest); resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	if err := startCamera(); err != nil {
		return err
	}
	s := first
	fresh := true
	for {
		if fresh {
			var observed any = s
			if s.Raw != nil {
				observed = s.Raw
			}
			if err := encoder.Encode(map[string]any{"time": time.Now(), "status": observed}); err != nil {
				return err
			}
		}
		if s.Printing() && s.Print.TaskID != "" && s.Print.TaskID != manifest.TaskID {
			manifest.Canceled = true
			break
		}
		if s.Idle() {
			manifest.Finished = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopped:
			camera = nil
			manifest.CameraFailures++
			if manifest.CameraFailures > retries {
				return errors.New("camera reconnect limit exceeded; partial recording retained")
			}
			fmt.Fprintln(log, "Camera stream ended; reconnecting into a new segment")
			if err := Pause(ctx, 2*time.Second); err != nil {
				return err
			}
			if err := startCamera(); err != nil {
				return err
			}
		case <-time.After(2 * time.Second):
		}
		next, err := c.Status(ctx)
		if err != nil {
			fresh = false
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(log, "Status unavailable; retrying")
			if err := encoder.Encode(map[string]any{"time": time.Now(), "error": "status unavailable"}); err != nil {
				return err
			}
			continue
		}
		fresh = true
		s = next
	}
	if !manifest.Canceled {
		if err := c.Snapshot(ctx, filepath.Join(directory, "finished.jpg")); err != nil {
			return fmt.Errorf("recorded video, but final snapshot failed: %w", err)
		}
	}
	return nil
}
