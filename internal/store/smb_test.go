package store

import (
	"docker-sync/internal/model"
	"testing"
)

func TestSMBPasswordMaskAndPreservation(t *testing.T) {
	s := openTestStore(t)
	in := s.GetSettings()
	in.SMB = model.SMBSettings{Enabled: true, Share: "//nas/backups", Username: "backup", Password: "secret"}
	saved, err := s.SetSettings(in)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SMB.Password != "" || !saved.SMB.HasPassword {
		t.Fatal("password exposed or flag absent")
	}
	if _, err = s.SetSettings(saved); err != nil {
		t.Fatal(err)
	}
	if s.SnapshotSettings().SMB.Password != "secret" {
		t.Fatal("password not preserved")
	}
	saved.SMB.Share = "//other/backups"
	s.SetSettings(saved)
	if s.SnapshotSettings().SMB.Password != "" {
		t.Fatal("credential reused for different share")
	}
	in.SMB.Password = "secret"
	s.SetSettings(in)
	saved = s.GetSettings()
	saved.SMB.HasPassword = false
	s.SetSettings(saved)
	if s.SnapshotSettings().SMB.Password != "" {
		t.Fatal("password not cleared")
	}
}
