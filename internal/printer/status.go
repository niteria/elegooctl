package printer

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Status struct {
	Raw         map[string]any `json:"-"`
	States      []int          `json:"CurrentStatus"`
	Coordinates string         `json:"CurrenCoord"`
	Bed         *float64       `json:"TempOfHotbed"`
	BedTarget   *float64       `json:"TempTargetHotbed"`
	Print       struct {
		Filename string `json:"Filename"`
		TaskID   string `json:"TaskId"`
		Layer    int    `json:"CurrentLayer"`
		Layers   int    `json:"TotalLayer"`
		Progress int    `json:"Progress"`
		Speed    int    `json:"PrintSpeedPct"`
	} `json:"PrintInfo"`
}

func (s Status) Printing() bool {
	for _, state := range s.States {
		if state == 1 {
			return true
		}
	}
	return false
}
func (s Status) Idle() bool       { return len(s.States) == 1 && s.States[0] == 0 }
func Filename(name string) string { return strings.TrimPrefix(name, "/local/") }

func (s Status) CheckJog(axis string, step float64) error {
	if !s.Idle() {
		return errors.New("printer is not idle; no move sent")
	}
	index := strings.Index("XYZ", axis)
	if len(axis) != 1 || index < 0 {
		return errors.New("axis must be X, Y or Z")
	}
	if math.IsNaN(step) || math.IsInf(step, 0) || step == 0 || math.Abs(step) > 100 {
		return errors.New("relative move must be finite, nonzero and at most 100 mm")
	}
	fields := strings.Split(s.Coordinates, ",")
	if len(fields) != 3 {
		return errors.New("printer coordinates are unavailable")
	}
	current, err := strconv.ParseFloat(fields[index], 64)
	if err != nil || math.IsNaN(current) || math.IsInf(current, 0) {
		return errors.New("invalid printer coordinates")
	}
	// Conservative bounds of the supported 256 mm machine. No implicit homing.
	if target := current + step; target < 5 || target > 250 {
		return fmt.Errorf("target %.2f is outside 5..250 mm", target)
	}
	return nil
}
