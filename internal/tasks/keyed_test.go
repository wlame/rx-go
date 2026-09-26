package tasks

import "testing"

// A keyed task shows one path and holds another key: a task on the path
// itself is no conflict, a second task for the key joins the first, and
// the key is free again once the task ends.
func TestManager_CreateKeyed_HoldsTheKeyNotThePath(t *testing.T) {
	m := New(Config{Logger: silentLogger()})
	chain, isNew := m.CreateKeyed("chain_index", "/var/log/syslog", "chain:/var/log/syslog")
	if !isNew || chain.Path != "/var/log/syslog" || chain.Operation != "chain_index" {
		t.Fatalf("CreateKeyed = %+v new=%v, want a new chain_index task shown as /var/log/syslog", chain, isNew)
	}

	file, isNew := m.Create("/var/log/syslog", "index")
	if !isNew || file.TaskID == chain.TaskID {
		t.Fatalf("an index task on the shown path was refused: %+v new=%v", file, isNew)
	}
	again, isNew := m.CreateKeyed("chain_index", "/var/log/syslog", "chain:/var/log/syslog")
	if isNew || again.TaskID != chain.TaskID {
		t.Fatalf("a second task for the key: %s new=%v, want the running %s", again.TaskID, isNew, chain.TaskID)
	}
	if holder, ok := m.Holder("chain:/var/log/syslog"); !ok || holder.TaskID != chain.TaskID {
		t.Fatalf("Holder(key) = %v, %v; want %s", holder, ok, chain.TaskID)
	}
	if holder, ok := m.Holder("/var/log/syslog"); !ok || holder.TaskID != file.TaskID {
		t.Fatalf("Holder(path) = %v, %v; want the index task %s", holder, ok, file.TaskID)
	}

	m.Complete(chain.TaskID, nil)
	if _, ok := m.Holder("chain:/var/log/syslog"); ok {
		t.Fatal("the key is still held after the task ended")
	}
	if next, isNew := m.CreateKeyed("chain_index", "/var/log/syslog", "chain:/var/log/syslog"); !isNew || next.TaskID == chain.TaskID {
		t.Fatalf("after the end: %s new=%v, want a new task", next.TaskID, isNew)
	}
}
