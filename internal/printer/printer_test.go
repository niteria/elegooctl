package printer

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	n, _ := strconv.Atoi(port)
	return &Client{Host: host, HTTPPort: n, SDCPPort: n, CameraPort: n, HTTP: server.Client()}
}

func wsHandler(t *testing.T, respond func(map[string]any) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		var message map[string]any
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Error(err)
			return
		}
		encoded, _ := json.Marshal(respond(message["Data"].(map[string]any)))
		if err := conn.Write(r.Context(), websocket.MessageText, encoded); err != nil {
			t.Error(err)
		}
	}
}

func TestSDCPStatusAndQuery(t *testing.T) {
	c := testClient(t, wsHandler(t, func(request map[string]any) any {
		if request["RequestID"] == "" {
			t.Error("missing request ID")
		}
		if request["Cmd"] == float64(0) {
			return map[string]any{"Status": map[string]any{"CurrentStatus": []int{0}, "TempOfHotbed": 25}}
		}
		return map[string]any{"Data": map[string]any{"Cmd": request["Cmd"], "RequestID": request["RequestID"], "Data": map[string]any{"files": []any{}}}}
	}))
	s, err := c.Status(context.Background())
	if err != nil || !s.Idle() || *s.Bed != 25 {
		t.Fatal(s, err)
	}
	if _, err := c.Query(context.Background(), 320, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(context.Background(), 128, nil); err == nil {
		t.Fatal("accepted a non-read-only command")
	}
}

func TestUploadChunksAndVerification(t *testing.T) {
	payload := bytes.Repeat([]byte("gcode\n"), 200000)
	var received []byte
	var posts int
	hash := md5.Sum(payload)
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("Offset") != strconv.Itoa(len(received)) || r.FormValue("S-File-MD5") != hex.EncodeToString(hash[:]) {
				t.Error("incorrect chunk metadata")
			}
			part, header, err := r.FormFile("File")
			if err != nil {
				t.Error(err)
				return
			}
			defer part.Close()
			if header.Filename != "a file.gcode" {
				t.Error(header.Filename)
			}
			chunk, _ := io.ReadAll(part)
			received = append(received, chunk...)
			posts++
			fmt.Fprint(w, `{"code":"000000","success":true}`)
		} else {
			if r.URL.Path != "/local/a file.gcode" {
				t.Error(r.URL.Path)
			}
			w.Write(received)
		}
	}))
	path := filepath.Join(t.TempDir(), "a file.gcode")
	os.WriteFile(path, payload, 0600)
	if err := c.Upload(context.Background(), path, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || !bytes.Equal(received, payload) {
		t.Fatal("missing or corrupt chunk")
	}
	if err := c.Upload(context.Background(), path, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if posts != 2 {
		t.Fatal("verify-only uploaded data")
	}
}

func TestUploadRejectsCorruptDownload(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "wrong") }))
	path := filepath.Join(t.TempDir(), "part.gcode")
	os.WriteFile(path, []byte("right"), 0600)
	if err := c.Upload(context.Background(), path, true, io.Discard); err == nil {
		t.Fatal("accepted wrong MD5")
	}
}

func sampleJPEG() []byte {
	var data bytes.Buffer
	jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	return data.Bytes()
}

func TestSnapshotMultipartAndNoOverwrite(t *testing.T) {
	data := sampleJPEG()
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(data))
		w.Write(data)
		fmt.Fprint(w, "\r\n--frame--\r\n")
	}))
	path := filepath.Join(t.TempDir(), "snapshot.jpg")
	if err := c.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(path)
	if !bytes.Equal(stored, data) {
		t.Fatal("wrong frame")
	}
	if err := c.Snapshot(context.Background(), path); err == nil {
		t.Fatal("overwrote snapshot")
	}
}

func TestWriteFilePreservesSymlink(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "target")
	link := filepath.Join(d, "link")
	os.WriteFile(target, []byte("keep"), 0600)
	os.Symlink(target, link)
	if err := WriteFile(link, []byte("replace")); err == nil {
		t.Fatal("replaced symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("changed target")
	}
}

func TestSnapshotCameraBoundaryCompatibility(t *testing.T) {
	for _, delimiter := range []string{"--frame", "----frame"} {
		t.Run(delimiter, func(t *testing.T) {
			data := sampleJPEG()
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=--frame")
				fmt.Fprintf(w, "%s\r\nContent-Type: image/jpeg\r\n\r\n", delimiter)
				w.Write(data)
				fmt.Fprintf(w, "\r\n%s--\r\n", delimiter)
			}))
			if err := c.Snapshot(context.Background(), filepath.Join(t.TempDir(), "frame.jpg")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func active() Status {
	s := Status{States: []int{1}, Coordinates: "100,100,50"}
	s.Print.Filename = "sample.gcode"
	s.Print.TaskID = "job-1"
	s.Print.Layers = 10
	return s
}
func cool(bed, target float64) Status {
	s := Status{States: []int{0}, Bed: &bed, BedTarget: &target}
	s.Print.Layer = 10
	return s
}

func TestReadiness(t *testing.T) {
	for _, test := range []struct {
		name      string
		sequence  []Status
		event     string
		completed bool
	}{
		{"not started", []Status{cool(25, 0), cool(25, 0), cool(25, 0)}, "", false},
		{"cold startup", []Status{active(), active(), active(), active()}, "", false},
		{"heater on", []Status{active(), cool(25, 60), cool(25, 60), cool(25, 60)}, "", false},
		{"warm bed", []Status{active(), cool(45, 0), cool(45, 0), cool(45, 0)}, "", false},
		{"three samples", []Status{active(), cool(29, 0), cool(29, 0), cool(29, 0)}, "removal-ready", true},
		{"warm resets", []Status{active(), cool(29, 0), cool(31, 0), cool(29, 0)}, "", false},
		{"NaN resets", []Status{active(), cool(29, 0), cool(math.NaN(), 0), cool(29, 0)}, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			watch := Readiness{Filename: "sample.gcode", Threshold: 30}
			var event *Event
			for _, s := range test.sequence {
				var err error
				event, err = watch.Observe(s)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.event == "" {
				if event != nil {
					t.Fatal(event)
				}
				return
			}
			if event == nil || event.Event != test.event || event.Completed != test.completed {
				t.Fatal(event)
			}
		})
	}
}

func TestReadinessNewJobCancelsAndStoppedDiffers(t *testing.T) {
	w := Readiness{Filename: "sample.gcode", Threshold: 30}
	w.Observe(active())
	s := active()
	s.Print.TaskID = "job-2"
	event, err := w.Observe(s)
	if err != nil || event == nil || event.Event != "canceled" {
		t.Fatal(event, err)
	}
	w = Readiness{Filename: "sample.gcode", Threshold: 30}
	w.Observe(active())
	s = cool(25, 0)
	s.Print.Layer = 3
	for i := 0; i < 2; i++ {
		w.Observe(s)
	}
	event, err = w.Observe(s)
	if err != nil || event == nil || event.Completed {
		t.Fatal(event, err)
	}
}

func TestJogChecks(t *testing.T) {
	s := active()
	s.States = []int{0}
	if err := s.CheckJog("X", 10); err != nil {
		t.Fatal(err)
	}
	for _, step := range []float64{0, 101, -100, math.NaN(), math.Inf(1)} {
		if err := s.CheckJog("X", step); err == nil {
			t.Fatalf("accepted %v", step)
		}
	}
	s.States = []int{1}
	if s.CheckJog("X", 10) == nil {
		t.Fatal("accepted movement during printing")
	}
}

// This helper is spawned instead of ffmpeg to exercise reconnect and cleanup.
func TestFFmpegHelper(t *testing.T) {
	if os.Getenv("ELEGOOCTL_TEST_FFMPEG") != "1" {
		return
	}
	marker := os.Getenv("ELEGOOCTL_TEST_MARKER")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		os.WriteFile(marker, []byte("failed"), 0600)
		os.Exit(1)
	}
	os.WriteFile(os.Args[len(os.Args)-1], []byte("simulated-video"), 0600)
	for {
		time.Sleep(time.Hour)
	}
}

func TestRecorderReconnectsAndKeepsManifest(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires POSIX shell")
	}
	bin := t.TempDir()
	exe, _ := os.Executable()
	script := "#!/bin/sh\nexec " + strconv.Quote(exe) + " -test.run=TestFFmpegHelper -- \"$@\"\n"
	os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ELEGOOCTL_TEST_FFMPEG", "1")
	t.Setenv("ELEGOOCTL_TEST_MARKER", filepath.Join(bin, "failed-once"))
	var queries atomic.Int32
	ws := wsHandler(t, func(_ map[string]any) any { queries.Add(1); return cool(25, 0) })
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/websocket" {
			ws(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(sampleJPEG())
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "recording")
	if err := c.Record(ctx, active(), dir, 2, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "recording.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recording Recording
	json.Unmarshal(data, &recording)
	if recording.CameraFailures != 1 || len(recording.Segments) != 2 || !recording.Finished || queries.Load() == 0 {
		t.Fatal(string(data))
	}
}

func TestHostValidation(t *testing.T) {
	for _, host := range []string{"", "http://example.invalid", "bad host", "user@example.invalid"} {
		c := Client{Host: host, SDCPPort: 3030, HTTPPort: 80, CameraPort: 3031}
		if c.Validate() == nil {
			t.Fatal(host)
		}
	}
}
