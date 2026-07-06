// Package j16 implements the OCPP 1.6 (JSON) message framing and message types.
//
// The wire format is a JSON array:
//
//	CALL        [2, "<uniqueId>", "<Action>", {payload}]
//	CALLRESULT  [3, "<uniqueId>", {payload}]
//	CALLERROR   [4, "<uniqueId>", "<errorCode>", "<errorDescription>", {errorDetails}]
//
// This file defines the frame types and the encode/decode functions used by
// both the simulator-side (charger) and the stub CMS.
package j16

import (
	"encoding/json"
	"errors"
	"fmt"
)

// MessageType is the first element of every OCPP-J frame.
type MessageType int

const (
	CALL       MessageType = 2
	CALLRESULT MessageType = 3
	CALLERROR  MessageType = 4
)

// Call is an outgoing or incoming CALL frame.
type Call struct {
	MessageID string
	Action    string
	Payload   json.RawMessage
}

// Marshal returns the wire bytes for a CALL.
func (c Call) Marshal() ([]byte, error) {
	payload := c.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return json.Marshal([4]any{int(CALL), c.MessageID, c.Action, payload})
}

// CallResult is the success response to a CALL.
type CallResult struct {
	MessageID string
	Payload   json.RawMessage
}

func (r CallResult) Marshal() ([]byte, error) {
	payload := r.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return json.Marshal([3]any{int(CALLRESULT), r.MessageID, payload})
}

// CallError is the failure response to a CALL.
type CallError struct {
	MessageID        string
	ErrorCode        string
	ErrorDescription string
	ErrorDetails     json.RawMessage
}

func (e CallError) Marshal() ([]byte, error) {
	details := e.ErrorDetails
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}
	return json.Marshal([5]any{int(CALLERROR), e.MessageID, e.ErrorCode, e.ErrorDescription, details})
}

// DecodedFrame is a tagged union — exactly one of Call/CallResult/CallError is non-nil.
type DecodedFrame struct {
	Type       MessageType
	Call       *Call
	CallResult *CallResult
	CallError  *CallError
}

// Decode parses a single OCPP-J frame from raw JSON bytes.
func Decode(data []byte) (*DecodedFrame, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("not a JSON array: %w", err)
	}
	if len(arr) < 3 {
		return nil, errors.New("frame too short")
	}
	var mt int
	if err := json.Unmarshal(arr[0], &mt); err != nil {
		return nil, fmt.Errorf("messageTypeId: %w", err)
	}
	var msgID string
	if err := json.Unmarshal(arr[1], &msgID); err != nil {
		return nil, fmt.Errorf("uniqueId: %w", err)
	}

	switch MessageType(mt) {
	case CALL:
		if len(arr) != 4 {
			return nil, fmt.Errorf("CALL: expected 4 elements, got %d", len(arr))
		}
		var action string
		if err := json.Unmarshal(arr[2], &action); err != nil {
			return nil, fmt.Errorf("action: %w", err)
		}
		return &DecodedFrame{
			Type: CALL,
			Call: &Call{MessageID: msgID, Action: action, Payload: arr[3]},
		}, nil

	case CALLRESULT:
		if len(arr) != 3 {
			return nil, fmt.Errorf("CALLRESULT: expected 3 elements, got %d", len(arr))
		}
		return &DecodedFrame{
			Type:       CALLRESULT,
			CallResult: &CallResult{MessageID: msgID, Payload: arr[2]},
		}, nil

	case CALLERROR:
		if len(arr) != 5 {
			return nil, fmt.Errorf("CALLERROR: expected 5 elements, got %d", len(arr))
		}
		var code, desc string
		if err := json.Unmarshal(arr[2], &code); err != nil {
			return nil, fmt.Errorf("errorCode: %w", err)
		}
		if err := json.Unmarshal(arr[3], &desc); err != nil {
			return nil, fmt.Errorf("errorDescription: %w", err)
		}
		return &DecodedFrame{
			Type: CALLERROR,
			CallError: &CallError{
				MessageID:        msgID,
				ErrorCode:        code,
				ErrorDescription: desc,
				ErrorDetails:     arr[4],
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown messageTypeId: %d", mt)
	}
}
