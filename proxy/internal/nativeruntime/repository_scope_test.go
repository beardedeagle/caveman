package nativeruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JuliusBrussee/caveman/engine/ccr"
	"github.com/JuliusBrussee/caveman/proxy/internal/gitsafe"
)

func repositoryGit(t *testing.T, root string, args ...string) {
	t.Helper()
	if output, err := gitsafe.Command(context.Background(), root, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func waitForRepositoryWarm(t *testing.T, runtime *Runtime) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.repositoryMu.RLock()
		warming := len(runtime.repositoryWarming)
		runtime.repositoryMu.RUnlock()
		if warming == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("repository warm did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRepositoryEvidenceRequiresSelectedGitRepository(t *testing.T) {
	collection := t.TempDir()
	repo := filepath.Join(collection, "selected")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	repositoryGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "src", "handler.go"), []byte("package main\nfunc Handle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "handler_test.go"), []byte("package main\nfunc TestHandle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := ccr.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := New(store)
	request := Request{
		ProtocolVersion: 1, Agent: Agent{ID: "codex"},
		Session: Session{ID: "selected-repo", CWD: collection}, Event: Event{Type: "session.start"},
		TaskProfile: &TaskProfile{Type: "general", Terms: []string{"handler"}},
	}
	if _, err := runtime.Handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitForRepositoryWarm(t, runtime)
	request.Event.Type = "prompt.submit"
	response, err := runtime.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	waitForRepositoryWarm(t, runtime)
	if strings.Contains(response.Context, "Likely implementation path") || response.RecoveryRef != "" {
		t.Fatalf("parent collection injected a neighboring repository: %+v", response)
	}
	objects, err := store.ListSessionObjects(request.Session.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if containsType(objects, ccr.ObjectRepositoryMap) || containsType(objects, ccr.ObjectEvidenceBundle) {
		t.Fatal("parent collection was scanned as a repository")
	}

	// An explicit CWD inside a repository must still warm and inject evidence,
	// including files at the repository root rather than just the nested CWD.
	request.Session.CWD = filepath.Join(repo, "src")
	if _, err := runtime.Handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitForRepositoryWarm(t, runtime)
	response, err = runtime.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Context, "src/handler.go") || response.RecoveryRef == "" {
		t.Fatalf("selected repository lost valid evidence: %+v", response)
	}
	// Root-relative maps still support edit impact from a nested CWD, even
	// after deleting the changed file.
	if err := os.Remove(filepath.Join(repo, "src", "handler.go")); err != nil {
		t.Fatal(err)
	}
	edit := request
	edit.Event.Type = "tool.after"
	edit.Tool = &Tool{Name: "apply_patch", Input: json.RawMessage(`{"path":"handler.go"}`), Output: []byte("deleted handler.go")}
	if _, err := runtime.Handle(context.Background(), edit); err != nil {
		t.Fatal(err)
	}
	objects, err = store.ListSessionObjects(request.Session.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundImpact := false
	for _, object := range objects {
		if object.Type == ccr.ObjectExecutionState && strings.Contains(string(object.Data), "src/handler_test.go") {
			foundImpact = true
		}
	}
	if !foundImpact {
		t.Fatal("nested-CWD deletion lost conservative test impact")
	}

	// A linked worktree has a .git file, not a directory. Selecting it in the
	// same session must replace the scope without reusing the old map handle.
	repositoryGit(t, repo, "add", ".")
	repositoryGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	worktree := filepath.Join(collection, "worktree")
	repositoryGit(t, repo, "worktree", "add", "--detach", worktree)
	if err := os.WriteFile(filepath.Join(worktree, "src", "worktree.go"), []byte("package main\nfunc WorktreeHandler() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	request.Session.CWD = filepath.Join(worktree, "src")
	request.TaskProfile.Terms = []string{"worktree"}
	if _, err := runtime.Handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitForRepositoryWarm(t, runtime)
	response, err = runtime.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Context, "src/worktree.go") || response.RecoveryRef == "" {
		t.Fatalf("selected worktree lost valid evidence: %+v", response)
	}
}

func TestRepositorySessionRetainsOnlyCurrentReferenceAndClearsOnEnd(t *testing.T) {
	root := t.TempDir()
	repositoryGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "handler.go"), []byte("package main\nfunc Handle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := ccr.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := New(store)
	request := Request{
		ProtocolVersion: 1, Agent: Agent{ID: "codex"},
		Session: Session{ID: "changing-state", CWD: root}, Event: Event{Type: "prompt.submit"},
		TaskProfile: &TaskProfile{Type: "bugfix", Terms: []string{"handler"}},
	}
	for _, state := range []string{"git:first", "git:second"} {
		request.Session.RepositoryState = state
		if _, err := runtime.Handle(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		waitForRepositoryWarm(t, runtime)
		response, err := runtime.Handle(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if response.RecoveryRef == "" {
			t.Fatalf("state %s has no evidence", state)
		}
		if len(runtime.repositorySessionRefs) != 1 {
			t.Fatalf("session retained %d map references, want only the current one", len(runtime.repositorySessionRefs))
		}
	}
	request.Event.Type = "session.end"
	if _, err := runtime.Handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(runtime.repositorySessionRefs) != 0 || len(runtime.repositoryWarming) != 0 {
		t.Fatal("ended session retained repository references or warmers")
	}
}

func TestRepositoryEvidenceRejectsRedirectedWorktree(t *testing.T) {
	for _, redirect := range []string{"sibling", "ancestor"} {
		t.Run(redirect, func(t *testing.T) {
			collection := t.TempDir()
			selected := filepath.Join(collection, "selected")
			foreign := filepath.Join(collection, "foreign")
			for _, path := range []string{selected, foreign} {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			repositoryGit(t, selected, "init", "-q")
			target := foreign
			if redirect == "ancestor" {
				target = collection
			}
			repositoryGit(t, selected, "config", "core.worktree", target)
			if err := os.WriteFile(filepath.Join(foreign, "handler.go"), []byte("package foreign\nfunc Handler() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := ccr.OpenMemory()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			runtime := New(store)
			request := Request{
				ProtocolVersion: 1, Agent: Agent{ID: "codex"},
				Session: Session{ID: "redirected", CWD: selected}, Event: Event{Type: "session.start"},
				TaskProfile: &TaskProfile{Type: "bugfix", Terms: []string{"handler"}},
			}
			if _, err := runtime.Handle(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			waitForRepositoryWarm(t, runtime)
			request.Event.Type = "prompt.submit"
			response, err := runtime.Handle(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			waitForRepositoryWarm(t, runtime)
			if response.RecoveryRef != "" || strings.Contains(response.Context, "handler.go") {
				t.Fatalf("core.worktree redirected evidence outside selected repository: %+v", response)
			}
			objects, err := store.ListSessionObjects(request.Session.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			if containsType(objects, ccr.ObjectRepositoryMap) {
				t.Fatal("redirected worktree was scanned")
			}
		})
	}
}
