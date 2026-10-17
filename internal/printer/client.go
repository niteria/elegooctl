// Package printer implements the LAN protocols used by supported Elegoo printers.
package printer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type Client struct {
	Host                           string
	SDCPPort, HTTPPort, CameraPort int
	HTTP                           *http.Client
}

func (c *Client) Validate() error {
	if c.Host == "" || strings.ContainsAny(c.Host, "/?#@ \t\r\n") {
		return errors.New("--host must be a hostname or IP address, without scheme or port")
	}
	for _, port := range []int{c.SDCPPort, c.HTTPPort, c.CameraPort} {
		if port < 1 || port > 65535 {
			return errors.New("port must be between 1 and 65535")
		}
	}
	return nil
}

func (c *Client) endpoint(scheme string, port int, path string) string {
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(c.Host, strconv.Itoa(port)), Path: path}).String()
}

func (c *Client) request(ctx context.Context, command int, data any, status bool) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, c.endpoint("ws", c.SDCPPort, "/websocket"), nil)
	if err != nil {
		return nil, fmt.Errorf("connect SDCP: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	requestID := hex.EncodeToString(id)
	body := map[string]any{"Id": "", "Data": map[string]any{
		"Cmd": command, "Data": data, "RequestID": requestID,
		"MainboardID": "", "TimeStamp": time.Now().UnixMilli(), "From": 1,
	}}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, encoded); err != nil {
		return nil, err
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read SDCP: %w", err)
		}
		var message map[string]any
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, err
		}
		if status {
			if found := findStatus(message); found != nil {
				return found, nil
			}
		} else if command == 1 {
			if attributes, ok := message["Attributes"].(map[string]any); ok {
				return attributes, nil
			}
		} else if response, ok := message["Data"].(map[string]any); ok && response["Cmd"] == float64(command) {
			if id, ok := response["RequestID"].(string); ok && id != "" && id != requestID {
				continue
			}
			return message, nil
		}
	}
}

func findStatus(value map[string]any) map[string]any {
	if _, ok := value["CurrentStatus"]; ok {
		return value
	}
	for _, item := range value {
		if child, ok := item.(map[string]any); ok {
			if result := findStatus(child); result != nil {
				return result
			}
		}
	}
	return nil
}

func (c *Client) RawStatus(ctx context.Context) (map[string]any, error) {
	return c.request(ctx, 0, map[string]any{}, true)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	raw, err := c.RawStatus(ctx)
	if err != nil {
		return Status{}, err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Status{}, err
	}
	var status Status
	err = json.Unmarshal(data, &status)
	return status, err
}

func (c *Client) Attributes(ctx context.Context) (map[string]any, error) {
	return c.request(ctx, 1, map[string]any{}, false)
}

// Query exposes only the known file/history read commands, never arbitrary commands.
func (c *Client) Query(ctx context.Context, command int, data map[string]any) (map[string]any, error) {
	switch command {
	case 258, 260, 320, 321, 323:
	default:
		return nil, fmt.Errorf("unsupported read-only query %d", command)
	}
	return c.request(ctx, command, data, false)
}

// Jog sends a single relative move. It does not home or try to recover disabled axes.
func (c *Client) Jog(ctx context.Context, axis string, step float64) error {
	status, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if err := status.CheckJog(axis, step); err != nil {
		return err
	}
	response, err := c.request(ctx, 401, map[string]any{"Axis": axis, "Step": step}, false)
	if err != nil {
		return err
	}
	data, _ := response["Data"].(map[string]any)
	payload, _ := data["Data"].(map[string]any)
	if payload["Ack"] != float64(0) {
		return fmt.Errorf("move rejected (Ack %v); Ack 3 means the axis requires homing", payload["Ack"])
	}
	return nil
}
