package session

import (
	"errors"
	"testing"
)

func TestNewInvariants(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s, err := New(NewInput{
			ID: "sess_1", OwnerTaskID: "tsk_1",
			CreatedAt: 100, FirstSeenAt: 100, LastSeenAt: 200,
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if s.ID() != "sess_1" || s.OwnerTaskID() != "tsk_1" {
			t.Fatalf("identity mismatch: %+v", s)
		}
		if s.LastSeenAt() != 200 || s.FirstSeenAt() != 100 {
			t.Fatalf("timestamps mismatch: first=%d last=%d", s.FirstSeenAt(), s.LastSeenAt())
		}
	})
	t.Run("empty ID rejected", func(t *testing.T) {
		if _, err := New(NewInput{OwnerTaskID: "t", FirstSeenAt: 1, LastSeenAt: 1}); err == nil {
			t.Fatal("expected err for empty ID")
		}
	})
	t.Run("empty OwnerTaskID rejected", func(t *testing.T) {
		if _, err := New(NewInput{ID: "s", FirstSeenAt: 1, LastSeenAt: 1}); err == nil {
			t.Fatal("expected err for empty OwnerTaskID")
		}
	})
	t.Run("LastSeenAt < FirstSeenAt rejected", func(t *testing.T) {
		if _, err := New(NewInput{ID: "s", OwnerTaskID: "t", FirstSeenAt: 200, LastSeenAt: 100}); err == nil {
			t.Fatal("expected err for last<first")
		}
	})
	t.Run("LastSeenAt == FirstSeenAt allowed", func(t *testing.T) {
		if _, err := New(NewInput{ID: "s", OwnerTaskID: "t", FirstSeenAt: 100, LastSeenAt: 100}); err != nil {
			t.Fatalf("equal timestamps should be allowed: %v", err)
		}
	})
}

func mustNew(id, owner string, first, last int64) *Session {
	s, err := New(NewInput{ID: ID(id), OwnerTaskID: owner, FirstSeenAt: first, LastSeenAt: last})
	if err != nil {
		panic(err)
	}
	return s
}

func TestClaimBy(t *testing.T) {
	t.Run("same owner lastSeen advances -> Changed", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 100)
		d := s.ClaimBy("tsk_1", 200, "")
		if !d.Changed || d.ConflictOwner != "" {
			t.Fatalf("decision = %+v, want Changed=true no conflict", d)
		}
		s.ApplyClaim("tsk_1", 200, "")
		if s.LastSeenAt() != 200 {
			t.Fatalf("lastSeenAt = %d, want 200", s.LastSeenAt())
		}
	})
	t.Run("same owner parentID changes -> Changed", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 100)
		d := s.ClaimBy("tsk_1", 100, "parent_2")
		if !d.Changed {
			t.Fatalf("expected Changed=true for parentID change")
		}
		s.ApplyClaim("tsk_1", 100, "parent_2")
		if s.ParentID() != "parent_2" {
			t.Fatalf("parentID = %q, want parent_2", s.ParentID())
		}
	})
	t.Run("same owner all equal -> not Changed", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		s.parentID = "p1"
		d := s.ClaimBy("tsk_1", 200, "p1")
		if d.Changed {
			t.Fatal("expected Changed=false for idempotent")
		}
	})
	t.Run("different owner -> ConflictOwner", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		d := s.ClaimBy("tsk_2", 300, "")
		if d.Changed {
			t.Fatal("expected Changed=false for conflict")
		}
		if d.ConflictOwner != "tsk_1" {
			t.Fatalf("ConflictOwner = %q, want tsk_1", d.ConflictOwner)
		}
	})
	t.Run("same owner parentID non-empty -> empty -> Changed and cleared", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		s.parentID = "parent_1"
		d := s.ClaimBy("tsk_1", 200, "")
		if !d.Changed || d.ConflictOwner != "" {
			t.Fatalf("decision = %+v, want Changed=true no conflict", d)
		}
		s.ApplyClaim("tsk_1", 200, "")
		if s.ParentID() != "" {
			t.Fatalf("parentID = %q, want empty after ApplyClaim cleared it", s.ParentID())
		}
	})
	t.Run("empty taskID -> no-op", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		d := s.ClaimBy("", 300, "")
		if d.Changed || d.ConflictOwner != "" {
			t.Fatalf("expected zero decision, got %+v", d)
		}
	})
}

func TestTouchBy(t *testing.T) {
	t.Run("advance -> Changed", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		d := s.TouchBy("tsk_1", 300)
		if !d.Changed {
			t.Fatal("expected Changed=true")
		}
		s.ApplyTouch(300)
		if s.LastSeenAt() != 300 {
			t.Fatalf("lastSeenAt = %d, want 300", s.LastSeenAt())
		}
	})
	t.Run("equal -> not Changed", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		d := s.TouchBy("tsk_1", 200)
		if d.Changed {
			t.Fatal("expected Changed=false for equal")
		}
	})
	t.Run("older -> not Changed", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		d := s.TouchBy("tsk_1", 100)
		if d.Changed {
			t.Fatal("expected Changed=false for older")
		}
	})
	t.Run("not owned -> NotOwned", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		d := s.TouchBy("tsk_2", 300)
		if !d.NotOwned {
			t.Fatal("expected NotOwned=true")
		}
		if d.Changed {
			t.Fatal("expected Changed=false for not owned")
		}
	})
}

func TestDeleteOwnership(t *testing.T) {
	t.Run("owner matches -> affected 1", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		if got := s.DeleteOwnership("tsk_1"); got != 1 {
			t.Fatalf("affected = %d, want 1", got)
		}
	})
	t.Run("owner mismatch -> affected 0", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		if got := s.DeleteOwnership("tsk_2"); got != 0 {
			t.Fatalf("affected = %d, want 0", got)
		}
	})
	t.Run("empty owner -> affected 0", func(t *testing.T) {
		s := mustNew("sess_1", "tsk_1", 100, 200)
		if got := s.DeleteOwnership(""); got != 0 {
			t.Fatalf("affected = %d, want 0", got)
		}
	})
}

func TestAmbiguousOwnerError(t *testing.T) {
	e := NewAmbiguousOwnerError("sess_1", []string{"tsk_1", "tsk_2"})
	if e.SessionID != "sess_1" {
		t.Fatalf("SessionID = %q", e.SessionID)
	}
	if len(e.Owners) != 2 {
		t.Fatalf("Owners len = %d", len(e.Owners))
	}
	if e.Error() == "" {
		t.Fatal("Error() should not be empty")
	}
	// errors.As 可识别（供 application 用 errors.Is/As 判定 fail-closed）。
	var target *AmbiguousOwnerError
	if !errors.As(e, &target) {
		t.Fatal("errors.As should match AmbiguousOwnerError")
	}
}

func TestFromObservation(t *testing.T) {
	s, err := FromObservation(Observation{
		ID: "sess_1", ParentID: "p1", CreatedAt: 100, UpdatedAt: 200,
	}, "tsk_1", 150)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if s.ParentID() != "p1" || s.CreatedAt() != 100 || s.FirstSeenAt() != 150 || s.LastSeenAt() != 200 {
		t.Fatalf("fields mismatch: %+v", s)
	}
}