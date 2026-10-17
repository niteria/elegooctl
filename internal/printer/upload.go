package printer

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func (c *Client) do(ctx context.Context, method, endpoint, contentType string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	return client.Do(request)
}

// Upload verifies the published file by downloading it. It never starts printing.
func (c *Client) Upload(ctx context.Context, path string, verifyOnly bool, log io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("upload requires a nonempty regular file")
	}
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	const chunkSize int64 = 1 << 20
	for offset := int64(0); !verifyOnly && offset < info.Size(); offset += chunkSize {
		length := min(chunkSize, info.Size()-offset)
		chunk := make([]byte, length)
		if _, err := file.ReadAt(chunk, offset); err != nil {
			return err
		}
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for key, value := range map[string]string{"Check": "1", "S-File-MD5": checksum,
			"Offset": strconv.FormatInt(offset, 10), "Uuid": uuid, "TotalSize": strconv.FormatInt(info.Size(), 10)} {
			if err := form.WriteField(key, value); err != nil {
				return err
			}
		}
		part, err := form.CreateFormFile("File", filepath.Base(path))
		if err != nil {
			return err
		}
		if _, err := part.Write(chunk); err != nil {
			return err
		}
		if err := form.Close(); err != nil {
			return err
		}
		response, err := c.do(ctx, "POST", c.endpoint("http", c.HTTPPort, "/uploadFile/upload"), form.FormDataContentType(), &body)
		if err != nil {
			return err
		}
		var result struct {
			Code    string `json:"code"`
			Success bool   `json:"success"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
		response.Body.Close()
		if err != nil {
			return err
		}
		if response.StatusCode != 200 || result.Code != "000000" || !result.Success {
			return fmt.Errorf("upload rejected at offset %d (HTTP %d, code %s)", offset, response.StatusCode, result.Code)
		}
		fmt.Fprintf(log, "Uploaded %d/%d bytes\n", offset+length, info.Size())
	}
	for attempt := 0; attempt < 15; attempt++ {
		response, err := c.do(ctx, "GET", c.endpoint("http", c.HTTPPort, "/local/"+filepath.Base(path)), "", nil)
		if err != nil {
			return err
		}
		if response.StatusCode == 404 && attempt < 14 {
			response.Body.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return fmt.Errorf("verification download HTTP %d", response.StatusCode)
		}
		verified := md5.New()
		size, err := io.Copy(verified, io.LimitReader(response.Body, info.Size()+1))
		response.Body.Close()
		if err != nil {
			return err
		}
		if size != info.Size() || hex.EncodeToString(verified.Sum(nil)) != checksum {
			return errors.New("downloaded file differs from local size/MD5")
		}
		fmt.Fprintf(log, "Verified %s: %d bytes, matching MD5. Print not started.\n", filepath.Base(path), size)
		return nil
	}
	return errors.New("uploaded file was not published")
}
