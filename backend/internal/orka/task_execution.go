package orka

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// TaskExecutionClient is the Task lifecycle surface used for placement recovery.
type TaskExecutionClient interface {
	TaskState(context.Context, string, string) (TaskState, error)
	DeleteTask(context.Context, string, string, string) error
}

// PrepareTaskExecution removes a non-successful Task when its worker placement
// changed. A successful Task remains reusable and returns skipApply=true.
func PrepareTaskExecution(ctx context.Context, client TaskExecutionClient, namespace, name string, desired map[string]any, poll time.Duration) (skipApply bool, err error) {
	if poll <= 0 {
		poll = time.Second
	}
	for {
		state, err := client.TaskState(ctx, namespace, name)
		if err != nil {
			return false, fmt.Errorf("read Task %s before apply: %w", name, err)
		}
		if !state.Exists || taskExecutionEqual(state.Execution, desired) {
			return false, nil
		}
		if state.Phase == "Succeeded" {
			return true, nil
		}
		if state.ResourceVersion == "" || state.UID == "" {
			return false, fmt.Errorf("task %s has incomplete object identity", name)
		}
		if err := client.DeleteTask(ctx, namespace, name, state.ResourceVersion); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return false, fmt.Errorf("delete Task %s after execution change: %w", name, err)
		}

		ticker := time.NewTicker(poll)
		for {
			current, err := client.TaskState(ctx, namespace, name)
			if err != nil {
				ticker.Stop()
				return false, fmt.Errorf("wait for Task %s deletion: %w", name, err)
			}
			if !current.Exists {
				ticker.Stop()
				return false, nil
			}
			if current.UID != state.UID {
				ticker.Stop()
				break
			}
			select {
			case <-ctx.Done():
				ticker.Stop()
				return false, fmt.Errorf("wait for Task %s deletion: %w", name, ctx.Err())
			case <-ticker.C:
			}
		}
	}
}

func taskExecutionEqual(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}
