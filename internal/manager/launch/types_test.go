package launch

import (
	"reflect"
	"testing"
)

func TestPlanRuntimeSocketCleanupFiles(t *testing.T) {
	plan := &Plan{
		Paths: RuntimePaths{
			QMPSocket:        "/run/qmp.sock",
			GuestAgentSocket: "/run/qga.sock",
			SSHReadySocket:   "/run/ssh-ready.sock",
			ControlSocket:    "/run/virtie.sock",
		},
		CleanupFiles: []string{"/run/virtiofs.sock"},
	}

	got := plan.RuntimeSocketCleanupFiles()
	want := []string{
		"/run/qmp.sock",
		"/run/qga.sock",
		"/run/ssh-ready.sock",
		"/run/virtie.sock",
		"/run/virtiofs.sock",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cleanup files: got %#v want %#v", got, want)
	}
}

func TestOptionsWaitMode(t *testing.T) {
	if got := (Options{SSH: true}).WaitMode(); got != WaitSSH {
		t.Fatalf("ssh wait mode: got %q want %q", got, WaitSSH)
	}
	if got := (Options{SSH: false}).WaitMode(); got != WaitVM {
		t.Fatalf("vm wait mode: got %q want %q", got, WaitVM)
	}
}

func TestPlanForWaitMode(t *testing.T) {
	plan := &Plan{Options: Options{SSH: false}}
	if got := PlanForWaitMode(plan, WaitAuto); got != plan {
		t.Fatalf("auto mode should keep original plan")
	}

	sshPlan := PlanForWaitMode(plan, WaitSSH)
	if sshPlan == plan {
		t.Fatalf("explicit ssh mode should clone plan")
	}
	if !sshPlan.Options.SSH {
		t.Fatalf("ssh mode should enable ssh option")
	}
	if plan.Options.SSH {
		t.Fatalf("original plan should remain unchanged")
	}

	vmPlan := PlanForWaitMode(&Plan{Options: Options{SSH: true}}, WaitVM)
	if vmPlan.Options.SSH {
		t.Fatalf("vm mode should disable ssh option")
	}
}
