package radiokind

import "testing"

func TestLookup(t *testing.T) {
	for _, id := range []string{"graywolf", "UVK5", " uvk5 "} {
		if _, ok := Lookup(id); !ok {
			t.Errorf("%q not found", id)
		}
	}
	if _, ok := Lookup("aprsd"); ok {
		t.Error("unknown kind found")
	}
	if k, _ := Lookup(Default); !k.NeedsGraywolf {
		t.Error("the default kind is graywolf")
	}
}

func TestAllIsACopy(t *testing.T) {
	a := All()
	a[0].ID = "changed"
	if All()[0].ID != Graywolf || len(IDs()) != len(a) {
		t.Fatal("All shares the registry")
	}
}
