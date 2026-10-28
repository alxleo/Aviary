package jobs

import "testing"

func TestStatusSnapshotsRemainStable(t *testing.T) {
	store := NewStore()
	store.Create("upload")
	store.Update("upload", "Running", "converting", map[string]string{"path": "notes.docx"})
	before, _ := store.Get("upload")
	store.Update("upload", "success", "uploaded", map[string]string{"path": "notes.pdf"})
	if before.Status != "Running" || before.Data["path"] != "notes.docx" {
		t.Fatalf("an in-flight status response changed: %+v", before)
	}
	after, _ := store.Get("upload")
	after.Status = "error"
	after.Data["path"] = "changed"
	current, _ := store.Get("upload")
	if current.Status != "success" || current.Data["path"] != "notes.pdf" {
		t.Fatalf("a response modified the stored result: %+v", current)
	}
}
