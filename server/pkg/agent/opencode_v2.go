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

// opencodeApplyWorkdirMCPConfig projects agent.mcp_config into
// <workdir>/opencode.json, the only per-task channel OpenCode 2.x still honours
// for MCP. OPENCODE_CONFIG_CONTENT (the 1.x channel), OPENCODE_CLI_CONFIG_CONTENT,
// OPENCODE_CONFIG and OPENCODE_CONFIG_DIR were each checked against 2.0.10, with
// and without --standalone, and none of them reach the session: the agent simply
// runs without the servers, with nothing logged.
//
// buildOpenCodeMCPConfigContent explains why the 1.x path injects env instead of
// writing here — the workdir is reused across turns for the same (agent, issue),
// and the agent or the user may own settings in this file. That reasoning still
// holds, so this function never owns the file, only the "mcp" key inside it:
// every other key is preserved as found, and "mcp" is removed again when
// mcp_config goes away so a stale server list cannot outlive its configuration.
//
// Rewriting through a map does not preserve the key order or formatting of a
// hand-edited file. That is accepted: JSON object order is not significant, and
// the alternative is a surgical editor for a file the daemon has to be able to
// both add to and clean up.
func opencodeApplyWorkdirMCPConfig(workdir string, raw json.RawMessage, logger *slog.Logger) error {
	var servers map[string]any
	if len(raw) > 0 {
		translated, err := translateMCPConfigForOpenCode(raw)
		if err != nil {
			return err
		}
		servers = translated
	}

	if workdir == "" {
		// No workdir means no project config file to write into. Only worth
		// saying anything when there was actually something to deliver.
		if len(servers) > 0 && logger != nil {
			logger.Warn("opencode: agent.mcp_config needs a task workdir on OpenCode 2.x; MCP servers were not applied",
				"servers", len(servers))
		}
		return nil
	}

	path := filepath.Join(workdir, opencodeWorkdirConfigName)
	doc := map[string]json.RawMessage{}
	switch existing, err := os.ReadFile(path); {
	case err == nil:
		if err := json.Unmarshal(existing, &doc); err != nil {
			// Refuse to overwrite a file that cannot be round-tripped: whatever
			// is in there belongs to the agent or the user. OpenCode would
			// reject it as well, so failing here turns an opaque CLI error into
			// one that names the file.
			return fmt.Errorf("opencode: %s is not valid JSON, refusing to overwrite it: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// No project config yet — the common case for a fresh workdir.
	default:
		return fmt.Errorf("opencode: read %s: %w", path, err)
	}

	if len(servers) == 0 {
		if _, present := doc["mcp"]; !present {
			return nil // nothing of ours to write and nothing to clean up
		}
		delete(doc, "mcp")
	} else {
		encoded, err := json.Marshal(servers)
		if err != nil {
			return fmt.Errorf("opencode mcp_config: marshal: %w", err)
		}
		doc["mcp"] = encoded
	}

	// Our key was the only thing in the file and it is now gone: remove the file
	// rather than leave an empty object where there was nothing before.
	if len(doc) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("opencode: remove %s: %w", path, err)
		}
		return nil
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encode %s: %w", path, err)
	}
	encoded = append(encoded, '\n')
	if err := writeFileAtomic(path, encoded); err != nil {
		return fmt.Errorf("opencode: write %s: %w", path, err)
	}
	return nil
}

// writeFileAtomic replaces path in one step so a reader (OpenCode itself, or the
// next turn in the same reused workdir) can never observe a half-written config.
func writeFileAtomic(path string, data []byte) error {
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
	// CreateTemp makes the file 0600; project config is not a secret and should
	// stay readable by the agent the same way a hand-written file would be.
	if err := os.Chmod(tmpName, 0o644); err != nil {
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
func opencodeInterruptSession(runtimeCmd Command, sessionID string, logger *slog.Logger) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), opencodeInterruptTimeout)
	defer cancel()

	// `opencode api` resolves the running service itself, which keeps the daemon
	// out of the business of discovering the service URL or its credentials.
	cmd := runtimeCmd.exec(ctx, "api", "POST", "/api/session/"+sessionID+"/interrupt")
	hideAgentWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if logger != nil {
			logger.Warn("opencode: session interrupt failed; the agent may keep running server-side",
				"session", sessionID, "error", err, "output", strings.TrimSpace(string(out)))
		}
		return
	}
	if logger != nil {
		logger.Info("opencode: session interrupted", "session", sessionID)
	}
}
