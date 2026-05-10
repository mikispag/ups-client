package notifier

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mikispag/ups-client/monitor"
)

// ShellTarget runs an external command on each matching event. Args may
// contain text/template fragments rendered against TemplateData. The child
// process inherits the parent environment plus UPS_* variables.
type ShellTarget struct {
	Label   string
	Command string
	Args    []string
	Env     map[string]string
	Timeout time.Duration
	Filter  Filter
}

// Name implements Notifier.
func (t *ShellTarget) Name() string {
	if t.Label != "" {
		return "shell:" + t.Label
	}
	return "shell:" + t.Command
}

// Match implements Notifier.
func (t *ShellTarget) Match(e monitor.Event) bool { return t.Filter.Match(e.Kind) }

// Notify implements Notifier.
func (t *ShellTarget) Notify(ctx context.Context, e monitor.Event) error {
	if t.Command == "" {
		return fmt.Errorf("shell target %q: empty command", t.Label)
	}
	td := NewTemplateData(e)

	args := make([]string, 0, len(t.Args))
	for i, raw := range t.Args {
		rendered, err := renderTemplate(fmt.Sprintf("%s.arg[%d]", t.Name(), i), raw, td)
		if err != nil {
			return err
		}
		args = append(args, rendered)
	}

	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, t.Command, args...)
	cmd.Env = append(os.Environ(), td.Env()...)
	for k, v := range t.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", t.Name(), err, string(out))
	}
	return nil
}
