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
func (s *scriptCheck) run() func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		timer := time.NewTimer(0)
		for {
			select {
			case <-ctx.Done():
				s.logger.Printf("[DEBUG] consul.checks: DONE<----------")
				// check has been removed
				return
			case <-s.shutdownCh:
				s.logger.Printf("[DEBUG] consul.checks: SHUTDOWN<----------")
				// Don't actually exit here, just make sure the timer ticks ASAP
				if timer.Stop() {
					timer.Reset(0)
				}
			case <-timer.C:
				s.logger.Printf("[DEBUG] consul.checks: TICK<----------")
				execctx, cancel := context.WithTimeout(ctx, s.check.Timeout)
				output, code, err := s.exec.Exec(execctx, s.check.Command, s.check.Args)
				cancel()
				switch execctx.Err() {
				case context.Canceled:
					// check removed during execution; exit
					return
				case context.DeadlineExceeded:
					s.logger.Printf("[DEBUG] consul.checks: check %q timed out (%s)", s.check.Name, s.check.Timeout)
				}
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
					timer.Reset(s.check.Interval)
				}
			}
		}
	}()
	return cancel
}
