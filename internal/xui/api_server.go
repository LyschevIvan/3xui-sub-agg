package xui

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

const MinimumPanelVersion = "3.4.2"

type ServerStatus struct {
	PanelVersion string `json:"panelVersion"`
}

type ServerAPI interface {
	Validate(context.Context) (ServerStatus, error)
}

func (c *APIClient) Validate(ctx context.Context) (ServerStatus, error) {
	status, err := doAPI[ServerStatus](
		ctx,
		c.transport,
		http.MethodGet,
		"server/status",
		nil,
		"",
	)
	if err != nil {
		return ServerStatus{}, err
	}
	if versionLess(status.PanelVersion, MinimumPanelVersion) {
		return ServerStatus{}, &Error{
			Kind:    ErrorUnsupportedVersion,
			Op:      "server status",
			Message: "3x-ui " + redact(status.PanelVersion, c.transport.token) + " is older than " + MinimumPanelVersion,
		}
	}
	return status, nil
}

func versionLess(version, minimum string) bool {
	current, currentOK := parsePanelVersion(version)
	baseline, baselineOK := parsePanelVersion(minimum)
	if !currentOK || !baselineOK || current[0] != 3 || baseline[0] != 3 {
		return true
	}
	if current[1] != baseline[1] {
		return current[1] < baseline[1]
	}
	return current[2] < baseline[2]
}

func parsePanelVersion(version string) ([3]int, bool) {
	if strings.HasPrefix(version, "v") {
		version = strings.TrimPrefix(version, "v")
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}

	var parsed [3]int
	for i, part := range parts {
		if part == "" {
			return [3]int{}, false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return [3]int{}, false
			}
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}, false
		}
		parsed[i] = value
	}
	return parsed, true
}
