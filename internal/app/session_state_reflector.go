package app

import (
	"errors"
	"reflect"

	"github.com/bwmarrin/discordgo"
)

// discordgo keeps the gateway resume state (unexported `sessionID` string
// and `sequence *int64` fields on its Session) private, but the reconnect
// guard needs to know whether a session still carries resume state so that
// clearing it for a fresh identify can be decided correctly. sessionState
// mirrors them via reflection, reading only; mutating fields belonging to a
// library type is fragile across upgrades, so writes go through
// clearSessionResumeState only, which validates the field shape once against
// a dummy session and skips silently if the shape changed.

// discordSessionFieldNames resolves the names of the unexported resume
// fields on the discordgo Session type, or the first error that explains why
// they cannot be found. Resolution is cheap (two FieldByName lookups) and
// only runs on the reconnect-guard slow paths.
func discordSessionFieldNames() (string, string, error) {
	sessionType := reflect.TypeFor[discordgo.Session]()

	sessionIDField, found := sessionType.FieldByName("sessionID")
	if !found {
		return "", "", errSessionIDFieldMissing
	}

	sequenceField, found := sessionType.FieldByName("sequence")
	if !found {
		return "", "", errSessionSequenceFieldMissing
	}

	return sessionIDField.Name, sequenceField.Name, nil
}

var (
	errSessionIDFieldMissing       = errors.New("discordgo session is missing sessionID field")
	errSessionSequenceFieldMissing = errors.New("discordgo session is missing sequence field")
)

// sessionStateReflectorReady reports whether the reflector can read the
// current discordgo session internals at all.
func sessionStateReflectorReady() bool {
	_, _, err := discordSessionFieldNames()

	return err == nil
}

type discordSessionState struct {
	sessionID string
	seq       int64
}

// sessionState reads the resume state (session ID and gateway sequence)
// from a discordgo session. It reports no state when the fields cannot be
// reflected (a discordgo version that changed their shape), so the guard
// degrades to its heartbeat-based path.
func sessionState(session *discordgo.Session) discordSessionState {
	state := discordSessionState{
		sessionID: "",
		seq:       0,
	}

	sessionIDFieldName, sequenceFieldName, err := discordSessionFieldNames()
	if err != nil {
		return state
	}

	sessionValue := reflect.ValueOf(session).Elem()

	sessionIDField := sessionValue.FieldByName(sessionIDFieldName)
	sequenceField := sessionValue.FieldByName(sequenceFieldName)

	session.RLock()
	defer session.RUnlock()

	if sessionIDField.IsValid() && sessionIDField.Kind() == reflect.String {
		state.sessionID = sessionIDField.String()
	}

	if sequenceField.IsValid() && sequenceField.Kind() == reflect.Pointer {
		sequenceValue := sequenceField.Elem()
		if sequenceValue.IsValid() && sequenceValue.Kind() == reflect.Int64 {
			state.seq = sequenceValue.Int()
		}
	}

	return state
}

// clearSessionResumeState forgets the gateway resume state on a session so
// the next connection identifies fresh. It is a no-op when the discordgo
// field shape has changed (the library already reconnects, just via resumes
// that may be rejected).
func clearSessionResumeState(session *discordgo.Session) {
	sessionIDFieldName, sequenceFieldName, err := discordSessionFieldNames()
	if err != nil {
		return
	}

	sessionValue := reflect.ValueOf(session).Elem()

	sessionIDField := sessionValue.FieldByName(sessionIDFieldName)
	sequenceField := sessionValue.FieldByName(sequenceFieldName)

	session.RLock()
	defer session.RUnlock()

	if sessionIDField.IsValid() && sessionIDField.CanSet() && sessionIDField.Kind() == reflect.String {
		sessionIDField.SetString("")
	}

	if sequenceField.IsValid() && sequenceField.CanSet() && sequenceField.Kind() == reflect.Pointer {
		if sequenceField.IsNil() {
			return
		}

		sequenceValue := sequenceField.Elem()
		if sequenceValue.IsValid() && sequenceValue.CanSet() && sequenceValue.Kind() == reflect.Int64 {
			sequenceValue.SetInt(0)
		}
	}
}
