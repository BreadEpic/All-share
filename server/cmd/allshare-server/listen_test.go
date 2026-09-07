package main

import "testing"

// Where the server binds decides whether a hosting platform can route traffic
// to it at all. A platform hands the port over in $PORT and reports the service
// unhealthy if nothing answers there, so the precedence here is worth pinning:
// an explicit setting always wins, $PORT fills the gap, and the local default
// applies when neither is set.
func TestDefaultListen(t *testing.T) {
	cases := []struct {
		name      string
		listenVar string
		portVar   string
		want      string
	}{
		{"nothing set", "", "", ":8443"},
		{"platform supplies PORT", "", "8080", ":8080"},
		{"PORT already has a colon", "", ":8080", ":8080"},
		{"PORT with stray whitespace", "", " 3000 ", ":3000"},
		{"explicit setting wins over PORT", "127.0.0.1:9000", "8080", "127.0.0.1:9000"},
		{"explicit setting with no PORT", ":1234", "", ":1234"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ALLSHARE_LISTEN", c.listenVar)
			t.Setenv("PORT", c.portVar)
			if got := defaultListen(); got != c.want {
				t.Errorf("defaultListen() = %q, want %q", got, c.want)
			}
		})
	}
}
