package trayapp

import "fmt"

type Action string

const (
	ActionStatus       Action = "status"
	ActionStart        Action = "start"
	ActionStop         Action = "stop"
	ActionRestart      Action = "restart"
	ActionEnableLogin  Action = "enable-login"
	ActionDisableLogin Action = "disable-login"
	ActionDoctor       Action = "doctor"
)

func CTRGoArgs(action Action) ([]string, error) {
	switch action {
	case ActionStatus:
		return []string{"service", "status"}, nil
	case ActionStart:
		return []string{"service", "start"}, nil
	case ActionStop:
		return []string{"service", "stop"}, nil
	case ActionRestart:
		return []string{"service", "restart"}, nil
	case ActionEnableLogin:
		return []string{"service", "enable-login"}, nil
	case ActionDisableLogin:
		return []string{"service", "disable-login"}, nil
	case ActionDoctor:
		return []string{"doctor"}, nil
	default:
		return nil, fmt.Errorf("unknown tray action: %s", action)
	}
}

func ServiceSetupArgs() []string {
	return []string{"service", "install", "--start", "--start-at-login"}
}
