package task

import (
	"fmt"
	"strings"
	"time"
)

// State (kind: string) represents the last known state of a task.
// A task can be in one of three states
// StateScheduled: this is the initial state of the task when it enters the queue.
// StateProcessing: once work begins on the task, it is put into this state.
// StateComplete: work is no longer being done on this task. client should check task result.
type State string

const (
	StateScheduled  State = "scheduled"
	StateProcessing State = "processing"
	StateComplete   State = "complete"
	StateCanceled   State = "canceled"
)

type Outcome string

const (
	OutcomeUnknown  Outcome = "unknown"
	OutcomeSuccess  Outcome = "success"
	OutcomeFailure  Outcome = "failure"
	OutcomeCanceled Outcome = "canceled"
)

// Type (kind: string) represents the kind of activity the daemon asked to perform. In alignment
// with the testground command-line we have two kinds of tasks
// TypeBuild -- which functions similarly to `testground build`. The result of this task will contain
// a build ID which can be used in a subsequent run.
// TypeRun -- which functions similarly to `testground run`
type Type string

const (
	TypeBuild Type = "build"
	TypeRun   Type = "run"
)

// DatedState (kind: struct) is a State with a timestamp.
type DatedState struct {
	Created time.Time `json:"created"`
	State   State     `json:"state"`
}

// Task (kind: struct) contains metadata about a testground task. This schema is used to store
// metadata in our task storage database as well as the wire format returned when clients get the
// state of a running or scheduled task.
type Task struct {
	Version     int          `json:"version"`     // Schema version
	Priority    int          `json:"priority"`    // Scheduling priority
	ID          string       `json:"id"`          // Unique identifier for this task
	Plan        string       `json:"plan"`        // Test plan
	Case        string       `json:"case"`        // Test case
	States      []DatedState `json:"states"`      // State of the task
	Type        Type         `json:"type"`        // Type of the task
	Composition interface{}  `json:"composition"` // Composition used for the task
	Input       interface{}  `json:"input"`       // The input data for this task
	Result      interface{}  `json:"result"`      // Result of the task, when terminal.
	Error       string       `json:"error"`       // Error from Testground
	CreatedBy   string       `json:"created_by"`  // Who created the task
}

func (t *Task) Created() time.Time {
	if len(t.States) == 0 {
		panic("task must have a state")
	}

	return t.States[0].Created
}

func (t *Task) IsCanceled() bool {
	return t.State().State == StateCanceled
}

func (t *Task) Name() string {
	return t.Plan + ":" + t.Case
}

func (t *Task) Took() time.Duration {
	return t.State().Created.Sub(t.Created()).Truncate(time.Second)
}

func (t *Task) State() DatedState {
	if len(t.States) == 0 {
		panic("task must have a state")
	}
	return t.States[len(t.States)-1]
}

func (t *Task) ParseCreatedBy() string {
	res := strings.Split(t.CreatedBy, ";")

	if len(res) == 2 {
		url := res[1]
		title := res[0]
		return fmt.Sprintf(`<a href="%s" target="_blank">%s</a>`, url, title)
	}

	return t.CreatedBy
}

// CreatedByCommit parses the task.CreatedBy field in case it contains a ;
// https://github.com/filecoin-project/oni/commit/d8b5a13f1fc758c674ffcee6dbd7f13540997283
// Currently CircleCI utilises the following format for the `user` field in the `.env.toml`
// user="filecoin-project/oni -> '$CIRCLE_BRANCH';https://github.com/filecoin-project/oni/commit/'$CIRCLE_SHA1'
func (t *Task) CreatedByCommit() (string, string, string, bool) {
	res := strings.Split(t.CreatedBy, ";")

	if len(res) == 2 {
		url := res[1]

		res = strings.Split(url, "/")
		if len(res) != 7 {
			fmt.Println(len(res))
			return "", "", "", false
		}

		owner := res[3]
		repo := res[4]
		commit := res[6]
		return owner, repo, commit, true
	}

	return "", "", "", false
}
