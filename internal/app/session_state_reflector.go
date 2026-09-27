package app

import (
	"errors"
	"reflect"

	"github.com/bwmarrin/discordgo"
)

// discordgo keeps the gateway resume state (unexported sessionID string and
// sequence *int64 fields on its Session) private by design: closing the
// session preserves them so the next Open resumes (Op 6 Resume), while a
// rejected resume makes the gateway answer Op 9 Invalid Session and
// discordgo re-identifies itself (Op 2 Identify). The reconnect guard only
// needs a read-only view of that state, so sessionState mirrors the two
// fields via reflection and never mutates library internals.
type discordSessionState struct {
	sessionID string
	seq       int64
}

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

// sessionState reads the resume state (session ID and gateway sequence)
// from a discordgo session. It reports no state when the fields cannot be
// reflected (a discordgo version that changed their shape), so the guard
// degrades to its heartbeat-based path.
func sessionState(session *discordgo.Session) discordSessionState {
	state := discordSessionState{
		sessionID: "",
		seq:       0,
	}

	if session == nil {
		return state
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

// clearSessionResumeState is intentionally a no-op. discordgo keeps the
// gateway resume state (unexported sessionID string and sequence *int64 on
// its Session) private; reflection cannot set those fields (CanSet is false
// without unsafe), and discordgo's CloseWithCode deliberately preserves them
// so the library's reconnect loop can resume (wsapi.go: CloseWithCode only
// closes the websocket and emits Disconnect; reconnect calls Open, which
// sends Op 6 Resume whenever sessionID/sequence are set, Op 2 Identify
// otherwise). A rejected resume is already handled by the library: the
// gateway answers Op 9 Invalid Session and discordgo re-identifies itself.
// The callers' intent — forcing a fresh identify on probe recovery — is met
// without touching resume state: Session.Close also triggers the reconnect
// loop, so the watchdog/awake paths only need to close the session and let
// the library resume or re-identify as the gateway directs.
func clearSessionResumeState(_ *discordgo.Session) {
}
