package actions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const (
	retryReservationFileName = "remediation_retry_state.json"
	retryReservationTTL      = 30 * time.Minute
)

type retryReservationState struct {
	Version      int                         `json:"version"`
	Reservations map[string]retryReservation `json:"reservations"`
}

type retryReservation struct {
	ID        string `json:"id"`
	PatchHash string `json:"patch_hash"`
	CreatedAt string `json:"created_at"`
	PriorURL  string `json:"prior_url,omitempty"`
	ResultURL string `json:"result_url,omitempty"`
}

func (s *Service) retryReservationPath() string {
	return filepath.Join(s.dataDir, retryReservationFileName)
}

func (s *Service) loadRetryReservations() *retryReservationState {
	state := &retryReservationState{Version: 1, Reservations: map[string]retryReservation{}}
	data, err := os.ReadFile(s.retryReservationPath())
	if err == nil {
		if json.Unmarshal(data, state) != nil || state.Version != 1 {
			return &retryReservationState{Version: 1, Reservations: map[string]retryReservation{}}
		}
	}
	if state.Reservations == nil {
		state.Reservations = map[string]retryReservation{}
	}
	return state
}

func (s *Service) saveRetryReservations(state *retryReservationState) error {
	return statefile.WriteJSON(s.retryReservationPath(), state)
}
