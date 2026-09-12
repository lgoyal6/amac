package health

import "testing"

func TestParseLaunchdStatus(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantLoaded bool
		wantExit   int
		wantRun    bool
	}{
		{
			name: "modern launchctl print output",
			output: `gui/501/com.user.agents-sync = {
	state = not running
	last exit code = 0
}`,
			wantLoaded: true,
		},
		{
			name: "legacy launchctl list output",
			output: `{
	"Label" = "com.user.agents-sync";
	"LastExitStatus" = 3;
	"PID" = 123;
}`,
			wantLoaded: true,
			wantExit:   3,
			wantRun:    true,
		},
		{
			name:     "unrelated output is not loaded",
			output:   "gui/501/com.example.other = {\nlast exit code = 0\n}",
			wantExit: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loaded, exit, running := parseLaunchdStatus("com.user.agents-sync", []byte(tt.output))
			if loaded != tt.wantLoaded || exit != tt.wantExit || running != tt.wantRun {
				t.Fatalf("parseLaunchdStatus() = (%v, %d, %v), want (%v, %d, %v)",
					loaded, exit, running, tt.wantLoaded, tt.wantExit, tt.wantRun)
			}
		})
	}
}
