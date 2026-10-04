package serverless

import "testing"

func TestDeletedPeerCannotAutoApprove(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		calls := 0
		d := &Discovery{cfg: Config{
			IsTrusted:    func(string) bool { return trusted },
			IsUnfriended: func(string) bool { return true },
			AutoAccept:   func(string, []string, string) bool { calls++; return true },
			OnPending:    func(string, []string, string) { calls++ },
		}}
		if ok, pending := d.trustPolicy(testPeerA, nil, ""); ok || pending {
			t.Fatal("deleted peer admitted")
		}
		if d.trustPolicyPassive(testPeerA, nil, "") {
			t.Fatal("deleted peer passively admitted")
		}
		if calls != 0 {
			t.Fatal("deleted peer triggered automatic approval or pending callback")
		}
	}
}
