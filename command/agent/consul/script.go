package consul

import (
	"context"
	"log"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// heartbeater is the subset of consul agent functionality needed by script
// checks to heartbeat
type heartbeater interface {
	UpdateTTL(id, output, status string) error
}

type scriptHandle struct {
	// cancel the script
	cancel func()
	done   chan struct{}
}

// wait returns a chan that's closed when the script exits
func (s *scriptHandle) wait() <-chan struct{} {
	return s.done
}

type scriptCheck struct {
	id    string
	check *structs.ServiceCheck
	exec  ScriptExecutor
	agent heartbeater

	logger     *log.Logger
	shutdownCh <-chan struct{}
}

func newScriptCheck(id string, check *structs.ServiceCheck, exec ScriptExecutor, agent heartbeater, logger *log.Logger, shutdownCh <-chan struct{}) *scriptCheck {
	return &scriptCheck{
		id:         id,
		check:      check,
		exec:       exec,
		agent:      agent,
		logger:     logger,
		shutdownCh: shutdownCh,
	}
}

// run this script check and return its cancel func. If the shutdownCh is
// closed the check will be run once more before exiting.
func (s *scriptCheck) run() *scriptHandle {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			// Block until check is removed, Nomad is shutting
			// down, or the check interval is up
			select {
			case <-ctx.Done():
				// check has been removed
				return
			case <-s.shutdownCh:
				// unblock but don't exit until after we heartbeat once more
			case <-timer.C:
				timer.Reset(s.check.Interval)
			}

			// Execute check script with timeout
			execctx, cancel := context.WithTimeout(ctx, s.check.Timeout)
			output, code, err := s.exec.Exec(execctx, s.check.Command, s.check.Args)
			switch execctx.Err() {
			case context.Canceled:
				// check removed during execution; exit
				return
			case context.DeadlineExceeded:
				s.logger.Printf("[WARN] consul.checks: check %q timed out (%s)", s.check.Name, s.check.Timeout)
			}
			cancel() // cleanup context
			state := api.HealthCritical
			switch code {
			case 0:
				state = api.HealthPassing
			case 1:
				state = api.HealthWarning
			}
			if err != nil {
				state = api.HealthCritical
				output = []byte(err.Error())
			}
			if err := s.agent.UpdateTTL(s.id, string(output), state); err != nil {
				//TODO Do something special on errors? Log? Retry faster?
				s.logger.Printf("[DEBUG] consul.checks: update for check %q failed: %v", s.check.Name, err)
			}
			select {
			case <-s.shutdownCh:
				// We've been told to exit
				return
			default:
			}
		}
	}()
	return &scriptHandle{cancel: cancel, done: done}
}
