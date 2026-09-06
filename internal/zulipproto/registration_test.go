package zulipproto

import "testing"

// TestRegistrationFingerprintIsOrderIndependent: neither event-type
// order nor narrow order means anything to the server, so a reordering
// must not throw away a resumable queue — a false mismatch costs every
// message posted before the fresh /register.
func TestRegistrationFingerprintIsOrderIndependent(t *testing.T) {
	a := RegistrationFingerprint([]string{"message", "reaction"}, [][2]string{{"stream", "a"}, {"stream", "b"}})
	b := RegistrationFingerprint([]string{"reaction", "message"}, [][2]string{{"stream", "b"}, {"stream", "a"}})
	if a != b {
		t.Fatalf("reordering changed the fingerprint:\n %s\n %s", a, b)
	}
	if c := RegistrationFingerprint([]string{"message"}, nil); c == a {
		t.Fatal("different registrations share a fingerprint")
	}
}

// TestRegistrationFingerprintNormalisesEmpty: nil and empty must
// fingerprint identically, or a cosmetic change in how main builds an
// empty narrow would discard a live queue.
func TestRegistrationFingerprintNormalisesEmpty(t *testing.T) {
	if a, b := RegistrationFingerprint(nil, nil), RegistrationFingerprint([]string{}, [][2]string{}); a != b {
		t.Fatalf("nil (%s) and empty (%s) differ", a, b)
	}
	if got, want := RegistrationFingerprint(nil, nil), `{"event_types":[],"narrow":[]}`; got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
	// Narrow order is by operator then operand, and the first element
	// alone decides when the operators differ.
	if got, want := RegistrationFingerprint(nil, [][2]string{{"stream", "z"}, {"is", "dm"}}),
		`{"event_types":[],"narrow":[["is","dm"],["stream","z"]]}`; got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
}

func TestDescribeRegistrationChange(t *testing.T) {
	want := RegistrationFingerprint([]string{"message", "update_message", "reaction"}, nil)
	for _, tc := range []struct {
		name, old, want string
	}{
		{
			// The production case: a queue registered before the
			// reactions feature existed.
			name: "added event type",
			old:  RegistrationFingerprint([]string{"message", "update_message"}, nil),
			want: "event types +reaction",
		},
		{
			name: "removed event type",
			old:  RegistrationFingerprint([]string{"message", "update_message", "reaction", "subscription"}, nil),
			want: "event types -subscription",
		},
		{
			name: "narrow changed",
			old:  RegistrationFingerprint([]string{"message", "update_message", "reaction"}, [][2]string{{"stream", "fleet"}}),
			want: "narrow stream:fleet → none",
		},
		{
			name: "both",
			old:  RegistrationFingerprint([]string{"message"}, [][2]string{{"stream", "fleet"}}),
			want: "event types +reaction +update_message, narrow stream:fleet → none",
		},
		{
			// Every host running an image that predates the
			// fingerprint. Unknown must read as different.
			name: "absent",
			old:  "",
			want: "the predecessor image recorded no registration, so it cannot be shown to match",
		},
		{
			name: "unreadable",
			old:  "{not json",
			want: "the predecessor image recorded an unreadable registration",
		},
		{
			name: "same content, different bytes",
			old:  `{"event_types": ["message", "reaction", "update_message"], "narrow": []}`,
			want: "the recorded registration is formatted differently",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescribeRegistrationChange(tc.old, want); got != tc.want {
				t.Fatalf("DescribeRegistrationChange() = %q, want %q", got, tc.want)
			}
		})
	}
	// Defensive: the wanted side is built in-process, but the function
	// is exported and must not panic on nonsense.
	if got := DescribeRegistrationChange(`{"event_types":[],"narrow":[]}`, "{not json"); got != "the wanted registration is unreadable" {
		t.Fatalf("unreadable want = %q", got)
	}
	// A narrow that gains entries names both sides.
	got := DescribeRegistrationChange(
		RegistrationFingerprint(nil, nil),
		RegistrationFingerprint(nil, [][2]string{{"stream", "a"}, {"stream", "b"}}))
	if got != "narrow none → stream:a+stream:b" {
		t.Fatalf("narrow diff = %q", got)
	}
}
