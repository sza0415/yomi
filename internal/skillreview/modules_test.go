package skillreview

import "testing"

func TestReviewModulesDefineStableOrder(t *testing.T) {
	modules := ReviewModules()
	if len(modules) != 5 {
		t.Fatalf("module count = %d, want 5", len(modules))
	}
	for index, module := range modules {
		if module.Order != index+1 {
			t.Errorf("module %s order = %d, want %d", module.ID, module.Order, index+1)
		}
		if module.ID == "" || module.Name == "" || module.Description == "" {
			t.Errorf("incomplete module = %+v", module)
		}
	}
	if modules[0].Status != ModuleActive {
		t.Errorf("first module status = %q, want active", modules[0].Status)
	}
	for _, module := range modules[1:] {
		if module.Status != ModulePlanned {
			t.Errorf("module %s status = %q, want planned", module.ID, module.Status)
		}
	}
}
