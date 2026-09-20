package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// OpenCode 2.0 reorganised the contract `opencode run` speaks. Four things the
// daemon depends on changed at once, and only the first announces itself:
//
//   - `--dir` was removed. The task workdir now comes from the process cwd,
//     which the backend already sets (together with PWD).
//   - `--variant` was removed. A model variant now rides along inside the model
//     string as `provider/model#variant`.
//   - OPENCODE_CONFIG_CONTENT is no longer honoured, so agent.mcp_config has to
//     reach OpenCode through <workdir>/opencode.json instead.
//   - `opencode run` became a thin client in front of a resident background
//     service, so signalling the client's process group no longer stops the
//     work that run started.
//
// Only the `--dir` removal fails loudly (`Unrecognized flag: --dir`, exit 1
// within ~130ms, GH #8586). The other three are silent: a run that merely drops
// `--dir` starts fine and then executes without its MCP servers, and cannot be
// cancelled. That is why they are handled together rather than one at a time.
//
// Everything else the backend relies on was checked against 2.0.10 and is
// unchanged across the two majors — the `--format json` event vocabulary,
// `--session` resume, `.opencode/skills/` discovery, AGENTS.md, and
// `--dangerously-skip-permissions` (still accepted and still honoured) — so
// none of it is branched on here.

// opencodeInterruptTimeout bounds the session-interrupt call made while a run
// is being cancelled. It is deliberately short: the interrupt is an extra step
// in front of the termination path that already works, so a service that does
// not answer promptly must not delay the signals behind it.
const opencodeInterruptTimeout = 5 * time.Second

// opencodeWorkdirConfigName is the project config file OpenCode reads out of
// the directory a run is anchored to.
const opencodeWorkdirConfigName = "opencode.json"

// opencodeUsesV2Contract reports whether the resolved OpenCode CLI speaks the
// 2.x contract described above.
//
// Scoped to BuiltinRuntime for the same reason opencodeSeparatesReasoning is: a
// custom runtime profile wraps a binary this package did not choose, and its
// reported version string does not establish which usage convention that binary
// speaks. Reading "2.x" off a wrapper that actually execs OpenCode 1.x would
// break a runtime that works today, so anything unrecognised keeps the 1.x
// argv. A custom profile pointed at OpenCode 2.x therefore stays broken until
// someone can map profiles onto capabilities directly — which is the same
// trade-off the reasoning check already makes, and strictly better than
// regressing working 1.x profiles on a guess.
func opencodeUsesV2Contract(cfg Config) bool {
	if !cfg.BuiltinRuntime {
		return false
	}
	// parseSemver scans for a semver token anywhere in the string, which is what
	// this needs: 2.x reports `opencode v2.0.10` where 1.x reports a bare
	// `1.18.31`, and extractVersionLine keeps whichever whole line it matched.
	version, err := parseSemver(strings.TrimSpace(cfg.CLIVersion))
	if err != nil {
		return false
	}
	return version.Major >= 2
}

// opencodeModelArg folds a thinking level into the model string for the 2.x
// contract, which has no `--variant` flag and instead reads the variant off the
// model as `provider/model#variant`.
//
// The second return reports whether the thinking level was representable. It is
// false only when there is a level but no model to attach it to: 2.x resolves
// the default model server-side, so there is no model string to append to and
// the level cannot be expressed at all. Callers warn rather than fail, because
// losing the reasoning effort is not worth failing an otherwise valid task.
func opencodeModelArg(model, thinkingLevel string) (string, bool) {
	if thinkingLevel == "" {
		return model, true
	}
	if model == "" {
		return model, false
	}
	// A model that already carries a variant was pinned deliberately (by
	// agent.model or custom_args); it wins over the agent-level thinking level
	// rather than growing a second `#` that OpenCode would reject.
	if strings.Contains(model, "#") {
		return model, true
	}
	return model + "#" + thinkingLevel, true
}

// opencodeWorkdirMCPInjection records exactly what the daemon added to
// <workdir>/opencode.json so the run can take back that and nothing else.
//
// It has to be this precise because the file is not the daemon's. The workdir is
// reused across turns and belongs to the user and the agent: the user may run
// their own MCP servers out of it, and the agent may edit the file mid-run. So
// the injection is recorded per server name, together with whatever value that
// name held beforehand, and withdrawal restores those values rather than
// rewriting the section.
type opencodeWorkdirMCPInjection struct {
	path string
	// names the daemon wrote, sorted for deterministic logs and tests.
	names []string
	// prior holds the pre-injection value of any name that already existed, so
	// withdrawal puts the user's server back instead of deleting it.
	prior map[string]json.RawMessage
	// fileExisted / mcpExisted / mode describe the file before injection, so a
	// file or section the daemon created is removed again and one it merely
	// borrowed is left as found.
	fileExisted bool
	mcpExisted  bool
	mode        fs.FileMode
}

// opencodeApplyWorkdirMCPConfig projects agent.mcp_config into
// <workdir>/opencode.json, the only per-task channel OpenCode 2.x still honours
// for MCP. OPENCODE_CONFIG_CONTENT (the 1.x channel), OPENCODE_CLI_CONFIG_CONTENT,
// OPENCODE_CONFIG and OPENCODE_CONFIG_DIR were each checked against 2.0.10, with
// and without --standalone, and none of them reach the session: the agent simply
// runs without the servers, with nothing logged.
//
// Returns the injection to withdraw when the run ends, or nil when nothing was
// written. A task with no mcp_config never touches the file at all — not even to
// tidy a pre-existing "mcp" section, which belongs to whoever put it there.
//
// The file is written 0600 for as long as the injection is present: MCP entries
// carry bearer headers, OAuth client secrets and environment values, and in
// local-directory mode this path is inside the user's own checkout.
func opencodeApplyWorkdirMCPConfig(workdir string, raw json.RawMessage, logger *slog.Logger) (*opencodeWorkdirMCPInjection, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	servers, err := translateMCPConfigForOpenCode(raw)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, nil
	}
	if workdir == "" {
		// No workdir means no project config file to write into.
		if logger != nil {
			logger.Warn("opencode: agent.mcp_config needs a task workdir on OpenCode 2.x; MCP servers were not applied",
				"servers", len(servers))
		}
		return nil, nil
	}

	path := filepath.Join(workdir, opencodeWorkdirConfigName)
	doc, mcp, state, err := readOpenCodeWorkdirConfig(path)
	if err != nil {
		return nil, err
	}

	for name, server := range servers {
		if previous, ok := mcp[name]; ok {
			state.prior[name] = previous
		}
		encoded, err := json.Marshal(server)
		if err != nil {
			return nil, fmt.Errorf("opencode mcp_config: marshal %q: %w", name, err)
		}
		mcp[name] = encoded
		state.names = append(state.names, name)
	}
	sort.Strings(state.names)

	if err := writeOpenCodeWorkdirConfig(path, doc, mcp, 0o600); err != nil {
		return nil, err
	}
	return state, nil
}

// withdraw removes the daemon's MCP entries from the workdir config, restoring
// any user entry the injection shadowed and the file mode it found.
//
// This runs when the process is gone, before the daemon's own end-of-task steps.
// It matters most in local-directory mode, where the workdir is the user's
// checkout and `git add -A` would otherwise commit the injected credentials onto
// the delivery branch (see commitEverything in execenv/local_worktree.go).
//
// Best effort: the run is already over, and a workdir the agent deleted or
// rewrote into something unparsable is not worth failing a finished task for.
func (s *opencodeWorkdirMCPInjection) withdraw(logger *slog.Logger) {
	if s == nil {
		return
	}
	doc, mcp, current, err := readOpenCodeWorkdirConfig(s.path)
	if err != nil {
		if logger != nil {
			logger.Warn("opencode: could not withdraw injected MCP config; it may contain credentials",
				"path", s.path, "error", err)
		}
		return
	}
	if !current.fileExisted {
		return // the agent removed it; nothing of ours is left on disk
	}

	for _, name := range s.names {
		if previous, ok := s.prior[name]; ok {
			mcp[name] = previous
		} else {
			delete(mcp, name)
		}
	}

	// Drop a section that only existed to hold the injection, and a file that
	// only existed to hold that section.
	if len(mcp) == 0 && !s.mcpExisted {
		delete(doc, "mcp")
		if len(doc) == 0 && !s.fileExisted {
			if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) && logger != nil {
				logger.Warn("opencode: could not remove the config file it created", "path", s.path, "error", err)
			}
			return
		}
	}

	mode := s.mode
	if !s.fileExisted {
		mode = 0o600
	}
	if err := writeOpenCodeWorkdirConfig(s.path, doc, mcp, mode); err != nil && logger != nil {
		logger.Warn("opencode: could not withdraw injected MCP config; it may contain credentials",
			"path", s.path, "error", err)
	}
}

// readOpenCodeWorkdirConfig loads the workdir project config, splitting the
// "mcp" section out of it, and records what it found for later withdrawal. A
// missing file reads as an empty document.
func readOpenCodeWorkdirConfig(path string) (map[string]json.RawMessage, map[string]json.RawMessage, *opencodeWorkdirMCPInjection, error) {
	doc := map[string]json.RawMessage{}
	mcp := map[string]json.RawMessage{}
	state := &opencodeWorkdirMCPInjection{path: path, prior: map[string]json.RawMessage{}, mode: 0o600}

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		state.fileExisted = true
		if err := json.Unmarshal(existing, &doc); err != nil {
			// Refuse to rewrite a file that cannot be round-tripped: whatever is
			// in there belongs to the agent or the user. OpenCode would reject it
			// as well, so failing here turns an opaque CLI error into one that
			// names the file.
			return nil, nil, nil, fmt.Errorf("opencode: %s is not valid JSON, refusing to overwrite it: %w", path, err)
		}
		if info, err := os.Stat(path); err == nil {
			state.mode = info.Mode().Perm()
		}
		if section, ok := doc["mcp"]; ok {
			state.mcpExisted = true
			if err := json.Unmarshal(section, &mcp); err != nil {
				return nil, nil, nil, fmt.Errorf("opencode: the mcp section of %s is not an object: %w", path, err)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		// No project config yet — the common case for a fresh workdir.
	default:
		return nil, nil, nil, fmt.Errorf("opencode: read %s: %w", path, err)
	}
	return doc, mcp, state, nil
}

// writeOpenCodeWorkdirConfig folds the mcp section back into doc and replaces
// path in one step, so a reader — OpenCode itself, or the next turn in the same
// reused workdir — can never observe a half-written config.
//
// Rewriting through a map does not preserve the key order or formatting of a
// hand-edited file. That is accepted: JSON object order is not significant, and
// the alternative is a surgical editor for a file the daemon has to be able to
// both add to and take back.
func writeOpenCodeWorkdirConfig(path string, doc, mcp map[string]json.RawMessage, mode fs.FileMode) error {
	if len(mcp) > 0 || func() bool { _, ok := doc["mcp"]; return ok }() {
		encoded, err := json.Marshal(mcp)
		if err != nil {
			return fmt.Errorf("opencode: encode mcp section: %w", err)
		}
		doc["mcp"] = encoded
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encode %s: %w", path, err)
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(path, encoded, mode)
}

// writeFileAtomic replaces path in one step. mode is applied to the temporary
// file before the rename, so the contents are never briefly world-readable.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// No-ops once the rename below succeeded.
		tmp.Close()
		os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// opencodeSessionTracker carries the session id observed on a run's event
// stream over to that run's cancellation handler, which needs it to interrupt
// the session server-side.
//
// It exists because the two live in different goroutines: the scanner learns the
// id from the first event that carries one, while the cancellation handler is
// parked on the context. Execute gives every run its own tracker, so two runs
// sharing a Backend value cannot see each other's session.
type opencodeSessionTracker struct {
	mu sync.Mutex
	id string
}

// set records the first session id seen. Safe on a nil tracker so backends built
// without one (every unit test that drives processEvents directly) need no
// special casing.
func (t *opencodeSessionTracker) set(id string) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id == "" {
		t.id = id
	}
}

// get returns the observed session id, or "" if no event carried one yet.
func (t *opencodeSessionTracker) get() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

// opencodeRunConnection is the connection context a run was launched with, so
// its interrupt reaches the same service the work is actually running on.
//
// `opencode run` accepts `--server <url>` (and users can pass one through
// agent.custom_args), and it resolves the default background service relative to
// the process environment and working directory. An interrupt that dropped any
// of that would report success against a different service while the real
// session kept running.
type opencodeRunConnection struct {
	cmd Command
	// server is the `--server` value the run was launched with, if any.
	server string
	// standalone reports that the run owns a private server. That server is a
	// child of the client, so the existing process-group signalling already
	// stops it and no interrupt is needed.
	standalone bool
	// env and dir mirror the run's process environment and working directory,
	// which is how the CLI discovers the default service.
	env []string
	dir string
}

// opencodeConnectionFromArgs reads the connection flags out of a run's final
// argv, after custom_args have been merged in.
func opencodeConnectionFromArgs(args []string) (server string, standalone bool) {
	for i, arg := range args {
		switch {
		case arg == "--standalone":
			standalone = true
		case arg == "--server" && i+1 < len(args):
			server = args[i+1]
		case strings.HasPrefix(arg, "--server="):
			server = strings.TrimPrefix(arg, "--server=")
		}
	}
	return server, standalone
}

// opencodeInterruptSession asks the OpenCode 2.x service to stop a session. On
// 2.x this is the only thing that actually stops a run.
//
// `opencode run` is a thin client: the work happens inside a background service
// that outlives it, so the SIGTERM→SIGKILL the backend sends to the client's
// process group leaves the agent running — still calling tools, still writing to
// the workdir — after Multica has already recorded the task as finished.
// Reproduced against 2.0.10: with the client confirmed dead, a shell command the
// agent had started went on to complete 35 seconds later.
//
// Best effort by construction. This runs in front of the existing termination
// path and never replaces it, so an unreachable service, a session id that was
// never observed, or a non-zero exit all degrade to exactly the behaviour
// without it. It deliberately does not use the run's context: by the time this
// is called that context is already cancelled, which is what triggered it.
func opencodeInterruptSession(conn opencodeRunConnection, sessionID string, logger *slog.Logger) {
	if sessionID == "" {
		return
	}
	if conn.standalone {
		// A private server dies with the process group below; interrupting the
		// default service here would signal an unrelated session.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), opencodeInterruptTimeout)
	defer cancel()

	args := []string{"api", "POST", "/api/session/" + sessionID + "/interrupt"}
	if conn.server != "" {
		args = append(args, "--server", conn.server)
	}
	cmd := conn.cmd.exec(ctx, args...)
	hideAgentWindow(cmd)
	// Same environment and working directory as the run, because that is what
	// the CLI uses to find the default background service.
	cmd.Env = conn.env
	cmd.Dir = conn.dir
	// combinedOutputOwned rather than cmd.CombinedOutput: the context timeout
	// alone does not bound this call. CombinedOutput waits for EOF on the output
	// pipes, and an `api` process that exits while a descendant still holds them
	// keeps the read blocked — with the termination path behind it stuck too.
	// runOwned puts the call in its own process tree, applies a WaitDelay to the
	// pipe wait, and kills whatever is left.
	out, err := combinedOutputOwned(cmd, logger)
	if err != nil {
		if logger != nil {
			logger.Warn("opencode: session interrupt failed; the agent may keep running server-side",
				"session", sessionID, "server", conn.server, "error", err, "output", strings.TrimSpace(string(out)))
		}
		return
	}
	if logger != nil {
		logger.Info("opencode: session interrupted", "session", sessionID, "server", conn.server)
	}
}
