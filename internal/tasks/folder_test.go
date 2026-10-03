package tasks

import "testing"

// A task carries the rx command it does once its creator sets it, on
// every copy Get returns; a task over a directory reports its files
// through Files once its worker gives the function, and a task over one
// file reports none.
func TestManager_CLICommandAndFileProgress(t *testing.T) {
	m := New(Config{})
	task, _ := m.Create("/var/log/app", "index")
	if task.CLICommand != "" || task.Files() != nil {
		t.Fatalf("a new task: %q %+v", task.CLICommand, task.Files())
	}
	if !m.SetCLICommand(task.TaskID, "rx index /var/log/app --recursive") {
		t.Fatal("SetCLICommand of a known task")
	}
	if !m.ReportFiles(task.TaskID, func() (int64, int64) { return 3, 10 }) {
		t.Fatal("ReportFiles of a known task")
	}
	got, _ := m.Get(task.TaskID)
	if got.CLICommand != "rx index /var/log/app --recursive" {
		t.Errorf("cli command %q", got.CLICommand)
	}
	if files := got.Files(); files == nil || *files != (FileProgress{Done: 3, Total: 10}) {
		t.Errorf("files %+v", files)
	}
	if m.SetCLICommand(unknownID, "rx") || m.ReportFiles(unknownID, func() (int64, int64) { return 0, 0 }) {
		t.Error("an unknown task was changed")
	}
	m.Fail(task.TaskID, "held by the test")
}

// unknownID names no task.
const unknownID = "00000000-0000-4000-8000-000000000000"

// An index task over a directory is cancellable: it holds its own key,
// so it runs beside an index task of a file of the same path, and Cancel
// ends it.
func TestManager_ACancellableIndexTaskCancels(t *testing.T) {
	m := New(Config{})
	folder, _, isNew := m.CreateCancellable("/var/log/app", "index")
	if !isNew {
		t.Fatal("no folder task")
	}
	if _, isNew := m.Create("/var/log/app", "index"); !isNew {
		t.Error("a task of the same path was refused beside the folder task")
	}
	if err := m.Cancel(folder.TaskID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got, _ := m.Get(folder.TaskID); got.Status != StatusCancelled {
		t.Errorf("status %s", got.Status)
	}
}
