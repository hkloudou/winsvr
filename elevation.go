package winsvr

import "errors"

// Token elevation types, from Windows' TOKEN_ELEVATION_TYPE in winnt.h.
// golang.org/x/sys/windows exports the TokenElevationType information class but
// not the values it returns, so they are spelled out here. They are declared
// without a build tag on purpose: it keeps chooseElevation testable on any
// platform, which is where the branching actually lives.
const (
	elevationDefault = 1 // UAC is off, or the user is not an administrator
	elevationFull    = 2 // already an elevated token
	elevationLimited = 3 // UAC filtered an administrator's token
)

// elevationChoice is what an elevated launch should do with the token a session
// hands back.
type elevationChoice int

const (
	// elevateImpossible: the signed-in user has no elevated token at all. A
	// configuration mistake rather than something waiting will fix.
	elevateImpossible elevationChoice = iota
	// elevateWithToken: the token is already elevated, so use it unchanged.
	elevateWithToken
	// elevateWithLinked: UAC handed out the filtered half of a split token, so
	// the elevated half has to be fetched through TokenLinkedToken.
	elevateWithLinked
)

// chooseElevation decides how to get an elevated token, from the elevation type
// Windows reports for the session's token and, where that is not conclusive,
// whether the token already carries administrator rights.
//
// The last case is the one worth being careful about. TokenElevationTypeDefault
// covers two situations that look identical here: UAC is switched off, in which
// case an administrator's token is already the full one; and the user is simply
// not an administrator, in which case no elevated token exists anywhere. Asking
// whether the token is elevated separates them. Guessing wrong in the second
// case and launching anyway would start a payload that quietly lacks the rights
// it was configured to need.
func chooseElevation(kind uint32, tokenIsElevated bool) elevationChoice {
	switch kind {
	case elevationFull:
		return elevateWithToken
	case elevationLimited:
		return elevateWithLinked
	default: // elevationDefault, and anything Windows might add later
		if tokenIsElevated {
			return elevateWithToken
		}
		return elevateImpossible
	}
}

// shouldAutoElevate reports whether a launch that failed should be tried again
// with the elevated token.
//
// Only one failure qualifies: Windows refusing to start a payload whose manifest
// requires administrator. That is not a guess about what the payload wants, it is
// the payload saying so in the only way the loader listens to. A launch that was
// already elevated is never retried, so this can fire at most once per Run.
//
// Note what this delegates. The payload arrives over a channel nothing
// authenticates, so with the retry enabled its own manifest decides whether it
// runs as administrator. That is the default, and DisableAutoElevate is how a
// deployment keeps the decision to itself.
func shouldAutoElevate(err error, alreadyElevated, disabled bool) bool {
	if alreadyElevated || disabled {
		return false
	}
	return errors.Is(err, ErrElevationRequired)
}
