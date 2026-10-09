package server

import "github.com/yurifrl/agents/internal/protocol"

// TaskPatch is a partial task update (the Go equivalent of patching a subset
// of Task fields). Only non-nil fields are applied.
type TaskPatch struct {
	Status        *protocol.TaskStatus
	Acknowledged  *bool
	StartedAt     *int64
	EndedAt       *int64
	Result        *string
	Error         *string
	SessionFile   *string
	Worker        *protocol.TaskWorkerMeta
	StopRequested *bool
	Model         *string
}

func (p TaskPatch) apply(t *protocol.Task) {
	if p.Status != nil {
		t.Status = *p.Status
	}
	if p.Acknowledged != nil {
		t.Acknowledged = *p.Acknowledged
	}
	if p.StartedAt != nil {
		t.StartedAt = new(*p.StartedAt)
	}
	if p.EndedAt != nil {
		t.EndedAt = new(*p.EndedAt)
	}
	if p.Result != nil {
		t.Result = new(*p.Result)
	}
	if p.Error != nil {
		t.Error = *p.Error
	}
	if p.SessionFile != nil {
		t.SessionFile = *p.SessionFile
	}
	if p.Worker != nil {
		t.Worker = p.Worker.Clone()
	}
	if p.StopRequested != nil {
		t.StopRequested = *p.StopRequested
	}
	if p.Model != nil {
		t.Model = *p.Model
	}
}
