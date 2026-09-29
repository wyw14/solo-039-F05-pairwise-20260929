package model

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
)

type Command struct {
	ID        uuid.UUID
	Campaign  uuid.UUID
	Revision  uuid.UUID
	Epoch     uint64
	Equipment string
	Action    string
	Setpoint  float64
}

type Acknowledgement struct {
	Command    uuid.UUID
	Campaign   uuid.UUID
	Revision   uuid.UUID
	Epoch      uint64
	Equipment  string
	State      string
	Value      float64
	ReceivedAt int64
}

func NewAcknowledgement(c Command, state string, value float64, receivedAt int64) Acknowledgement {
	return Acknowledgement{Command: c.ID, Campaign: c.Campaign, Revision: c.Revision, Epoch: c.Epoch, Equipment: c.Equipment, State: state, Value: value, ReceivedAt: receivedAt}
}

func NewCommand(c Campaign, equipment, action string, setpoint float64) Command {
	return Command{ID: uuid.New(), Campaign: c.ID, Revision: c.Revision, Epoch: c.Epoch, Equipment: equipment, Action: action, Setpoint: setpoint}
}

func (c Command) Validate() error {
	if c.ID == uuid.Nil || c.Campaign == uuid.Nil || c.Revision == uuid.Nil {
		return errors.New("command identity is incomplete")
	}
	if c.Epoch == 0 {
		return errors.New("command epoch is required")
	}
	if c.Equipment == "" || c.Action == "" {
		return errors.New("command target is incomplete")
	}
	return nil
}

func (a Acknowledgement) Matches(command Command) bool {
	return a.Command == command.ID && a.Campaign == command.Campaign && a.Revision == command.Revision && a.Epoch == command.Epoch && a.Equipment == command.Equipment
}

func (a Acknowledgement) Validate(command Command) error {
	if err := command.Validate(); err != nil {
		return err
	}
	if !a.Matches(command) {
		return errors.New("acknowledgement belongs to another command generation")
	}
	if a.State == "" {
		return errors.New("acknowledgement state is required")
	}
	return nil
}

func (c Command) Key() string {
	return fmt.Sprintf("%s/%s/%d/%s/%s", c.Campaign, c.Revision, c.Epoch, c.Equipment, c.Action)
}
