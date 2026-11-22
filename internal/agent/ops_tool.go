package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// tool runs one of the allow-listed tools in a shape its policy accepts (see
// toolpolicy.go): the tool, its arguments, its environment and its output
// file are all checked against what the panel sends.
func (s *Server) tool(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
	plan, err := s.planTool(&toolCall{name: req.Name, args: req.Args, env: req.Env, mode: modeTool, outputFile: req.OutputFile})
	if err != nil {
		return nil, err
	}
	defer plan.close()
	timeout := 10 * time.Minute
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &tailBuffer{max: 512 * 1024}
	if plan.native != nil {
		code := plan.native(rctx, strings.NewReader(req.Stdin), out)
		return &ToolResponse{ExitCode: code, Output: out.String()}, nil
	}
	bin, err := resolveTool(req.Name)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: err.Error()}
	}
	if plan.prepare != nil {
		if err := plan.prepare(); err != nil {
			return nil, &Error{Message: req.Name + ": " + err.Error()}
		}
	}
	run := planRunner(rctx, plan, bin, req.Stdin, out)
	var code int
	if plan.wrap != nil {
		code, err = plan.wrap(rctx, run, out)
	} else {
		code, err = run(plan.args, nil)
	}
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			// a refusal the wrapper came to (restic restore: the snapshot)
			if ae.Output == "" {
				ae.Output = out.String()
			}
			return nil, ae
		}
		return nil, &Error{Message: req.Name + ": " + err.Error(), Output: out.String()}
	}
	return &ToolResponse{ExitCode: code, Output: out.String()}, nil
}

// planRunner runs the plan's program, bin, with the arguments it is given:
// with the plan's environment and files, in its sandbox when the plan has
// one, stdin as its input and its output (unless the plan or the caller take
// stdout) and errors into out.
func planRunner(ctx context.Context, plan *toolPlan, bin, stdin string, out io.Writer) toolRun {
	return func(args []string, stdout io.Writer) (int, error) {
		path, argv, env, files := bin, args, plan.env, plan.files
		if plan.confine != nil {
			c, err := plan.confine(ctx, bin, args, out)
			if err != nil {
				return 0, err
			}
			defer c.done()
			path, argv, env, files = c.path, c.args, append(append([]string{}, plan.env...), c.env...), c.files
		}
		cmd := exec.CommandContext(ctx, path, argv...)
		cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
		cmd.Env = append(cmd.Env, env...)
		cmd.ExtraFiles = files
		cmd.WaitDelay = 5 * time.Second
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		cmd.Stderr = out
		switch {
		case stdout != nil:
			cmd.Stdout = stdout
		case plan.stdout != nil:
			cmd.Stdout = plan.stdout
		default:
			cmd.Stdout = out
		}
		return exitCode(cmd.Run())
	}
}

// exitCode turns the result of cmd.Run into the tool's exit status; only a
// failure to run at all is an error.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// hasOtherNames reports whether the file has more than one name (a hard link).
func hasOtherNames(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Nlink > 1
}
