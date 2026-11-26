package store

import (
	"encoding/json"
	"testing"
)

func TestModDependencyUnmarshalLegacyAndCurrentForms(t *testing.T) {
	var old ModDependency
	if err := json.Unmarshal([]byte(`"carryonlib"`), &old); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if old.ModID != "carryonlib" || old.ResolvedID != 0 {
		t.Errorf("old = %+v", old)
	}

	var current ModDependency
	if err := json.Unmarshal([]byte(`{"modId":"dep","version":"2.0.0","resolvedId":42}`), &current); err != nil {
		t.Fatalf("current unmarshal: %v", err)
	}
	if current.ModID != "dep" || current.Version != "2.0.0" || current.ResolvedID != 42 {
		t.Errorf("current = %+v", current)
	}
}
