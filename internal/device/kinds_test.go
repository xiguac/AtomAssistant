package device

import "testing"

func TestKinds_粗略位置可以持续上报而精确位置不行(t *testing.T) {
	coarse, ok := KindByID("location.coarse")
	if !ok || coarse.PerUse {
		t.Fatalf("粗略位置应当登记为可持续上报: %+v", coarse)
	}
	for _, id := range []string{"location.now", "location.track"} {
		k, _ := KindByID(id)
		if !k.PerUse {
			t.Errorf("%s 是精确位置，必须按次授权", id)
		}
	}
	seen := map[string]bool{}
	for _, k := range Kinds {
		if seen[k.ID] {
			t.Errorf("数据种类 %s 登记了两次", k.ID)
		}
		seen[k.ID] = true
		if k.Role != RoleDecision && k.Role != RoleArchive {
			t.Errorf("%s 没有标用途", k.ID)
		}
	}
}
