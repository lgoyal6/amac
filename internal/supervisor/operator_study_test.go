package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

type operatorTaskDefinition struct {
	ID   string `json:"id"`
	Case string `json:"case"`
}

type operatorStudyTask struct {
	ID             string `json:"id"`
	Case           string `json:"case"`
	Expected       string `json:"expected"`
	Path           string `json:"path"`
	GrantPath      string `json:"grantPath,omitempty"`
	RequestSession string `json:"sessionId"`
	Content        string `json:"content"`
}

type operatorModeResult struct {
	Mode           string           `json:"mode"`
	Tasks          int              `json:"tasks"`
	Completed      int              `json:"completed"`
	Refused        int              `json:"refused"`
	Correct        int              `json:"correctDecisions"`
	Effects        int              `json:"effects"`
	IntentEvents   int              `json:"intentEvents"`
	EffectEvents   int              `json:"effectEvents"`
	Actuation      int              `json:"actuationEvents"`
	ManualCommands []string         `json:"manualCommands,omitempty"`
	TaskResults    []map[string]any `json:"taskResults"`
}

func TestM4OperatorModesThirtySyntheticTasks(t *testing.T) {
	defs := loadOperatorTaskDefinitions(t)
	if len(defs) != 30 {
		t.Fatalf("frozen operator suite has %d tasks, want 30", len(defs))
	}
	root := t.TempDir()
	manual, err := runManualBaseline(root, defs)
	if err != nil {
		t.Fatal(err)
	}
	recommendation := runOperatorMode(t, root, defs, "recommendation")
	execution := runOperatorMode(t, root, defs, "execution")
	for _, got := range []operatorModeResult{manual, recommendation, execution} {
		if got.Tasks != 30 || got.Completed != 6 || got.Refused != 24 || got.Correct != 30 {
			t.Fatalf("%s decision totals violate the frozen task oracle: %+v", got.Mode, got)
		}
	}
	if manual.Effects != 6 || recommendation.Effects != 0 || recommendation.IntentEvents != 0 || recommendation.EffectEvents != 0 || recommendation.Actuation != 0 ||
		execution.Effects != 6 || execution.IntentEvents != 6 || execution.EffectEvents != 6 || execution.Actuation != 6 {
		t.Fatalf("unexpected mode effect/audit totals: manual=%+v recommendation=%+v execution=%+v", manual, recommendation, execution)
	}
	out := map[string]any{
		"study":       "S3 synthetic local operator comparison",
		"taskCount":   len(defs),
		"runtime":     map[string]string{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH},
		"modes":       []operatorModeResult{manual, recommendation, execution},
		"metrics":     []string{"task outcome counts", "refusal counts", "filesystem effect counts", "durable intent/effect/actuation event counts"},
		"limitations": []string{"manual arm is an exact-command local baseline, not a human study; no human time is measured", "synthetic disposable paths and fake ACP child only", "no real chats, provider calls, user files, or external tools"},
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	outPath := os.Getenv("S3_M4_OUTPUT")
	if outPath == "" {
		outPath = filepath.Join(root, "operator-study.json")
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadOperatorTaskDefinitions(t *testing.T) []operatorTaskDefinition {
	t.Helper()
	if path := os.Getenv("S3_M4_TASKS"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var defs []operatorTaskDefinition
		if err := json.Unmarshal(b, &defs); err != nil {
			t.Fatal(err)
		}
		return defs
	}
	defs := make([]operatorTaskDefinition, 0, 30)
	cases := []string{"authorized_exact_path", "scope_mismatch", "no_grant", "wrong_session", "relative_path"}
	for i := 0; i < 30; i++ {
		defs = append(defs, operatorTaskDefinition{ID: fmt.Sprintf("M4-%02d", i+1), Case: cases[i%len(cases)]})
	}
	return defs
}

func materializeOperatorTasks(root string, defs []operatorTaskDefinition) []operatorStudyTask {
	tasks := make([]operatorStudyTask, 0, len(defs))
	for _, def := range defs {
		target := filepath.Join(root, def.ID, "result.txt")
		task := operatorStudyTask{ID: def.ID, Case: def.Case, Expected: "refuse", Path: target,
			RequestSession: "fake-session-1", Content: "synthetic payload " + def.ID}
		switch def.Case {
		case "authorized_exact_path":
			task.Expected, task.GrantPath = "complete", target
		case "scope_mismatch":
			task.GrantPath = target + ".outside"
		case "no_grant":
		case "wrong_session":
			task.GrantPath, task.RequestSession = target, "another-session"
		case "relative_path":
			task.Path = filepath.Join("relative", def.ID+".txt")
		default:
			panic("unrecognized frozen operator task case: " + def.Case)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func runManualBaseline(root string, defs []operatorTaskDefinition) (operatorModeResult, error) {
	tasks := materializeOperatorTasks(filepath.Join(root, "manual"), defs)
	result := operatorModeResult{Mode: "manual_command_baseline", Tasks: len(tasks), TaskResults: make([]map[string]any, 0, len(tasks))}
	for _, task := range tasks {
		row := map[string]any{"taskId": task.ID, "case": task.Case, "expected": task.Expected, "path": task.Path}
		if task.Expected == "complete" {
			command := "mkdir -p " + shellQuote(filepath.Dir(task.Path)) + " && printf '%s' " + shellQuote(task.Content) + " > " + shellQuote(task.Path)
			cmd := exec.Command("sh", "-c", command)
			if output, err := cmd.CombinedOutput(); err != nil {
				return result, fmt.Errorf("manual command failed for %s: %w: %s", task.ID, err, output)
			}
			content, err := os.ReadFile(task.Path)
			if err != nil || string(content) != task.Content {
				return result, fmt.Errorf("manual command did not create expected file for %s: %v", task.ID, err)
			}
			result.Completed++
			result.Effects++
			result.Correct++
			result.ManualCommands = append(result.ManualCommands, command)
			row["outcome"] = "completed"
			row["command"] = command
			row["effectApplied"] = true
		} else {
			result.Refused++
			result.Correct++
			row["outcome"] = "refused by the frozen authorization oracle"
			row["effectApplied"] = false
		}
		result.TaskResults = append(result.TaskResults, row)
	}
	return result, nil
}

func runOperatorMode(t *testing.T, root string, defs []operatorTaskDefinition, mode string) operatorModeResult {
	t.Helper()
	tasks := materializeOperatorTasks(filepath.Join(root, mode), defs)
	taskFile := filepath.Join(root, mode+"-tasks.json")
	b, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(root, mode+"-results.json")
	t.Setenv("AMAC_FAKE_TASKS", taskFile)
	t.Setenv("AMAC_FAKE_RESULTS", resultFile)
	sup := withFakeAgent(t, "operator-study")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", root)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()
	for _, task := range tasks {
		if sess.writeScope != nil {
			if err := sess.RevokeWrite(sess.writeScope.AuthorizationID); err != nil {
				t.Fatal(err)
			}
		}
		if task.GrantPath != "" {
			if err := sess.AuthorizeWrite(WriteScope{SessionID: sess.ID, AuthorizationID: "grant-" + task.ID,
				Path: task.GrantPath, ExpiresAt: time.Now().Add(5 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := sess.Prompt(ctx, mode+":"+task.ID); err != nil {
			t.Fatalf("%s task %s prompt: %v", mode, task.ID, err)
		}
	}
	var observations []map[string]any
	b, err = os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &observations); err != nil {
		t.Fatal(err)
	}
	if len(observations) != len(tasks) {
		t.Fatalf("%s returned %d results for %d tasks", mode, len(observations), len(tasks))
	}
	result := operatorModeResult{Mode: mode, Tasks: len(tasks), TaskResults: make([]map[string]any, 0, len(tasks))}
	for i, task := range tasks {
		obs := observations[i]
		row := map[string]any{"taskId": task.ID, "case": task.Case, "expected": task.Expected, "path": task.Path, "response": obs}
		_, hasError := obs["errorCode"]
		if !hasError {
			if mode == "recommendation" {
				var plan WritePlan
				raw, _ := json.Marshal(obs["result"])
				if err := json.Unmarshal(raw, &plan); err != nil || !plan.DryRun || plan.ToolCall.Params.Path != task.Path {
					t.Fatalf("recommendation %s omitted its exact dry-run plan: %s (%v)", task.ID, raw, err)
				}
				if _, err := os.Stat(task.Path); !os.IsNotExist(err) {
					t.Fatalf("recommendation %s caused a filesystem effect: %v", task.ID, err)
				}
			} else {
				content, err := os.ReadFile(task.Path)
				if err != nil || string(content) != task.Content {
					t.Fatalf("execution %s missing expected effect: %v", task.ID, err)
				}
				result.Effects++
			}
			result.Completed++
			if task.Expected == "complete" {
				result.Correct++
			} else {
				t.Fatalf("%s accepted invalid task %s", mode, task.ID)
			}
			row["outcome"] = "completed"
			row["effectApplied"] = mode == "execution"
		} else {
			result.Refused++
			if task.Expected != "refuse" {
				t.Fatalf("%s refused valid task %s: %v", mode, task.ID, obs)
			}
			result.Correct++
			row["outcome"] = "refused"
			row["effectApplied"] = false
		}
		result.TaskResults = append(result.TaskResults, row)
	}
	if mode == "recommendation" {
		result.IntentEvents = len(kinds(t, sup.log, event.KindOperationIntent))
		result.EffectEvents = len(kinds(t, sup.log, event.KindOperationEffect))
		result.Actuation = len(kinds(t, sup.log, event.KindActuation))
	} else {
		result.IntentEvents = len(kinds(t, sup.log, event.KindOperationIntent))
		result.EffectEvents = len(kinds(t, sup.log, event.KindOperationEffect))
		result.Actuation = len(kinds(t, sup.log, event.KindActuation))
	}
	return result
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
