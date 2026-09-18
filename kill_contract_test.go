package main

import "testing"

func TestKillRejectsInactiveContainersWithoutChangingExit(t *testing.T) {
	for _, started := range []bool{false, true} {
		api, c := contractAPI(t, 0)
		if started {
			c.start()
			c.stop(42)
		}
		before := c.stateSnapshot()
		response := contractRequest(api, "POST", "/containers/contract/kill", "")
		if response.Code != 409 {
			t.Errorf("inactive kill status=%d want409", response.Code)
		}
		if after := c.stateSnapshot(); after != before {
			t.Errorf("inactive kill mutated state: before=%+v after=%+v", before, after)
		}
	}
}

func TestKillSignalsAreNotSilentlyTreatedAsSIGKILL(t *testing.T) {
	for _, signal := range []string{"", "KILL", "SIGKILL", "9"} {
		api, c := contractAPI(t, 0)
		c.start()
		response := contractRequest(api, "POST", "/containers/contract/kill?signal="+signal, "")
		if response.Code != 204 || c.stateSnapshot().ExitCode != 137 {
			t.Fatalf("kill %q: %d %+v", signal, response.Code, c.stateSnapshot())
		}
	}
	for _, signal := range []string{"TERM", "SIGTERM", "15", "HUP", "0", "bogus"} {
		api, c := contractAPI(t, 0)
		c.start()
		response := contractRequest(api, "POST", "/containers/contract/kill?signal="+signal, "")
		if response.Code != 501 || response.Header().Get("X-Docker-Zero-Unsupported") != "true" || !c.isRunning() {
			t.Fatalf("unsupported signal %s changed state or claimed success: %d", signal, response.Code)
		}
	}
}
