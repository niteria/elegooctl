package printer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

type Event struct {
	Event     string    `json:"event"`
	Filename  string    `json:"filename"`
	TaskID    string    `json:"task_id"`
	Completed bool      `json:"completed"`
	Bed       float64   `json:"bed_temperature"`
	Time      time.Time `json:"time"`
}

// Readiness is job-locked; it never treats a later print as the selected job.
type Readiness struct {
	Filename, TaskID                    string
	Threshold                           float64
	Layers, Stable                      int
	Seen, Finished, Completed, Canceled bool
}

func (r *Readiness) Observe(s Status) (*Event, error) {
	if s.Printing() {
		r.Stable = 0
		if r.Seen && s.Print.TaskID != "" && s.Print.TaskID != r.TaskID {
			r.Canceled = true
			return &Event{Event: "canceled", Filename: r.Filename, TaskID: r.TaskID, Time: time.Now()}, nil
		}
		if Filename(s.Print.Filename) == Filename(r.Filename) && s.Print.TaskID != "" && (r.TaskID == "" || s.Print.TaskID == r.TaskID) {
			r.Seen = true
			r.TaskID = s.Print.TaskID
			r.Layers = s.Print.Layers
		}
		return nil, nil
	}
	if !r.Seen || !s.Idle() {
		r.Stable = 0
		return nil, nil
	}
	if r.Layers <= 0 {
		return nil, errors.New("selected job's expected layer count is unavailable")
	}
	r.Finished = true
	if s.Print.Layer >= r.Layers {
		r.Completed = true
	}
	if s.Bed == nil || s.BedTarget == nil || math.IsNaN(*s.Bed) || math.IsNaN(*s.BedTarget) || *s.Bed < 5 || *s.Bed > r.Threshold || math.Abs(*s.BedTarget) > 0.1 {
		r.Stable = 0
		return nil, nil
	}
	r.Stable++
	if r.Stable < 3 {
		return nil, nil
	}
	return &Event{Event: "removal-ready", Filename: r.Filename, TaskID: r.TaskID, Completed: r.Completed, Bed: *s.Bed, Time: time.Now()}, nil
}

// WaitJob attaches to an explicitly named upcoming/current job, or the current job.
func (c *Client) WaitJob(ctx context.Context, filename string) (Status, error) {
	for {
		s, err := c.Status(ctx)
		if err == nil && s.Printing() && s.Print.TaskID != "" && (filename == "" || Filename(s.Print.Filename) == Filename(filename)) {
			return s, nil
		}
		if filename == "" {
			if err != nil {
				return Status{}, err
			}
			return Status{}, errors.New("no current print; use --file to wait for a specific job")
		}
		if err := Pause(ctx, 2*time.Second); err != nil {
			return Status{}, err
		}
	}
}

func Pause(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(duration):
		return nil
	}
}

func (c *Client) WaitReady(ctx context.Context, first Status, threshold float64, progress func(Status, error)) (*Event, error) {
	if math.IsNaN(threshold) || threshold < 20 || threshold > 35 {
		return nil, fmt.Errorf("cooling threshold must be between 20 and 35 C")
	}
	r := Readiness{Filename: Filename(first.Print.Filename), TaskID: first.Print.TaskID, Threshold: threshold}
	if _, err := r.Observe(first); err != nil {
		return nil, err
	}
	for {
		if err := Pause(ctx, 5*time.Second); err != nil {
			return nil, err
		}
		s, err := c.Status(ctx)
		if progress != nil {
			progress(s, err)
		}
		if err != nil {
			r.Stable = 0
			continue
		}
		event, err := r.Observe(s)
		if err != nil || event != nil {
			return event, err
		}
	}
}
