package core

import (
	"testing"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

func commandCount(redis *mockMessagingClient, channel, command string) int {
	redis.mu.Lock()
	defer redis.mu.Unlock()
	n := 0
	for _, sent := range redis.sendCommands {
		if sent.channel == channel && sent.command == command {
			n++
		}
	}
	return n
}

func TestLockHibernateSubmitsOnePowerRequest(t *testing.T) {
	for _, state := range []librefsm.StateID{fsm.StateParked, fsm.StateStandby} {
		t.Run(string(state), func(t *testing.T) {
			v, _, redis := lockSemanticsSystem(t, state, true)
			if err := v.handleStateRequest("lock-hibernate"); err != nil {
				t.Fatal(err)
			}
			if v.machine.CurrentState() != state {
				t.Fatal("compatibility entry point bypassed PM admission")
			}
			if commandCount(redis, "scooter:power", "hibernate-manual") != 1 {
				t.Fatal("did not submit exactly one manual request")
			}
		})
	}
}

func TestHibernatePreparationStateMatrix(t *testing.T) {
	for _, state := range []librefsm.StateID{fsm.StateParked, fsm.StateStandby, fsm.StateReadyToDrive, fsm.StateHopOn, fsm.StateWaitingSeatbox, fsm.StateHibernationConfirm} {
		t.Run(string(state), func(t *testing.T) {
			v, io, redis := lockSemanticsSystem(t, state, true)
			redis.hashFields = map[string]string{"power-manager/hibernate-request-id": "test-request"}
			if err := v.handleStateRequest("prepare-hibernate:test-request"); err != nil {
				t.Fatal(err)
			}
			if state != fsm.StateParked {
				if v.machine.CurrentState() != state {
					t.Fatal("preparation changed a non-parked state")
				}
				if commandCount(redis, "scooter:power", "hibernate-preparation-failed:test-request") != 1 {
					t.Fatal("missing preparation failure response")
				}
				return
			}
			if v.machine.CurrentState() != fsm.StateShuttingDown || io.getDigitalOutput("engine_power") {
				t.Fatal("preparation did not enter graceful vehicle shutdown")
			}
			if countPublishedMessages(redis, "dbc:command", "poweroff") != 1 {
				t.Fatal("dashboard was not asked to halt")
			}
			sendLockEvent(t, v, fsm.EvShutdownTimeout)
			if v.machine.CurrentState() != fsm.StateStandby || io.getDigitalOutput("dashboard_power") {
				t.Fatal("shutdown did not finish in standby")
			}
			if commandCount(redis, "scooter:power", "hibernate-manual") != 0 {
				t.Fatal("vehicle preparation could overwrite PM's wake duration")
			}
		})
	}
}

func TestCancelledOrSupersededPreparationDoesNotLock(t *testing.T) {
	for _, current := range []string{"", "replacement"} {
		v, _, redis := lockSemanticsSystem(t, fsm.StateParked, true)
		redis.hashFields = map[string]string{"power-manager/hibernate-request-id": current}
		if err := v.handleStateRequest("prepare-hibernate:obsolete"); err != nil {
			t.Fatal(err)
		}
		if v.machine.CurrentState() != fsm.StateParked || countPublishedMessages(redis, "dbc:command", "poweroff") != 0 {
			t.Fatal("obsolete preparation shut down the vehicle")
		}
	}
}

func TestPhysicalHibernateSubmitsAfterStandbyOnce(t *testing.T) {
	v, _, redis := lockSemanticsSystem(t, fsm.StateHibernationConfirm, true)
	sendLockEvent(t, v, fsm.EvHibernationFinalTimeout)
	if commandCount(redis, "scooter:power", "hibernate-manual") != 0 {
		t.Fatal("physical request submitted before standby")
	}
	sendLockEvent(t, v, fsm.EvShutdownTimeout)
	if v.machine.CurrentState() != fsm.StateStandby {
		t.Fatal("physical confirmation did not shut down")
	}
	if commandCount(redis, "scooter:power", "hibernate-manual") != 1 {
		t.Fatal("physical confirmation must submit one request")
	}
	redis.mu.Lock()
	last := redis.publishedStates[len(redis.publishedStates)-1]
	redis.mu.Unlock()
	if last != types.StateStandby {
		t.Fatal("request was not preceded by standby publication")
	}
	v.completeHibernationShutdown()
	if commandCount(redis, "scooter:power", "hibernate-manual") != 1 {
		t.Fatal("shutdown completion repeated physical request")
	}
}

func TestUnlockDuringHibernatePreparationCancelsPowerIntent(t *testing.T) {
	v, _, redis := lockSemanticsSystem(t, fsm.StateParked, true)
	redis.hashFields = map[string]string{"power-manager/hibernate-request-id": "request"}
	if err := v.handleStateRequest("prepare-hibernate:request"); err != nil {
		t.Fatal(err)
	}
	if err := v.handleStateRequest("unlock"); err != nil {
		t.Fatal(err)
	}
	if commandCount(redis, "scooter:power", "hibernate-cancel") != 1 {
		t.Fatal("deferred unlock left the hibernation request active")
	}
	v.mu.RLock()
	pending := v.hibernationRequest || v.physicalHibernationRequest
	v.mu.RUnlock()
	if pending {
		t.Fatal("unlock left a physical completion request armed")
	}
}

func TestHibernateDoesNotRemoveDashboardInstallBlock(t *testing.T) {
	v, io, redis := lockSemanticsSystem(t, fsm.StateParked, true)
	redis.hashFields = map[string]string{
		"power-manager/hibernate-request-id": "request",
		"power:inhibits/install:dbc":         `{"type":"block"}`,
	}
	v.mu.Lock()
	v.dbcUpdating = true
	v.mu.Unlock()
	if err := v.handleStateRequest("prepare-hibernate:request"); err != nil {
		t.Fatal(err)
	}
	if countPublishedMessages(redis, "dbc:command", "poweroff") != 0 {
		t.Fatal("hibernation interrupted a dashboard installation")
	}
	sendLockEvent(t, v, fsm.EvShutdownTimeout)
	if !io.getDigitalOutput("dashboard_power") {
		t.Fatal("dashboard install lost power")
	}
	redis.mu.Lock()
	defer redis.mu.Unlock()
	for _, id := range redis.removedInhibitors {
		if id == "install:dbc" || id == "dbc-update" {
			t.Fatalf("removed update owner's inhibitor %s", id)
		}
	}
}
