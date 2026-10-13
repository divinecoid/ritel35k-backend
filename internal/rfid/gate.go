package rfid

import (
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/security"
)

// Decision outcomes.
const (
	Allow  = "allow"
	Alarm  = "alarm"
	Ignore = "ignored"
)

// TagFacts is what the gate decision needs to know about a tag (nil when the system has never seen it).
type TagFacts struct {
	Status  string
	StoreID *uuid.UUID
	SoldAt  *time.Time
}

type Rules struct {
	SoldGrace     time.Duration
	ReportUnknown bool
}

type Decision struct {
	Result   string // Allow | Alarm | Ignore
	Reason   string
	Type     string // security event type when Result == Alarm or a reportable anomaly
	Severity string
	Record   bool // write a security event
}

// DecideGate is the authoritative exit decision, made by the server for every gate read (the edge makes the same
// call offline from its snapshot, see edge/internal/rfid). It is a pure function of the tag's state at read time.
//
//	direction "in"            → ignored (people walking in with their purchases)
//	SOLD, sold before the read (+grace) → allow
//	SOLD, but sold only after the read  → UNAUTHORIZED_EXIT (left before paying)
//	in stock / shelf / reserved / returned / registered → UNAUTHORIZED_EXIT
//	lost / damaged tag seen leaving     → RFID_MISMATCH (a flagged tag is moving)
//	tag belongs to another store        → RFID_MISMATCH (allow when it was paid for)
//	unknown tag                         → allow; UNKNOWN_TAG only when the store opted in
func DecideGate(tag *TagFacts, readerStore uuid.UUID, direction string, readAt time.Time, r Rules) Decision {
	if direction == "in" {
		return Decision{Result: Ignore, Reason: "entering"}
	}
	if tag == nil {
		if r.ReportUnknown {
			return Decision{Result: Alarm, Reason: "unknown tag", Type: security.UnknownTag, Severity: "info", Record: true}
		}
		return Decision{Result: Allow, Reason: "unknown tag (not reported)"}
	}
	if tag.Status == Sold {
		if tag.SoldAt == nil || !tag.SoldAt.After(readAt.Add(r.SoldGrace)) {
			return Decision{Result: Allow, Reason: "sold"}
		}
		return Decision{Result: Alarm, Reason: "left before the sale was recorded", Type: security.UnauthorizedExit, Severity: "critical", Record: true}
	}
	if tag.StoreID != nil && *tag.StoreID != readerStore {
		return Decision{Result: Alarm, Reason: "tag belongs to another store", Type: security.RFIDMismatch, Severity: "warning", Record: true}
	}
	switch tag.Status {
	case Lost, Damaged:
		return Decision{Result: Alarm, Reason: "tag flagged " + tag.Status + " is leaving", Type: security.RFIDMismatch, Severity: "warning", Record: true}
	default:
		return Decision{Result: Alarm, Reason: "unpaid item (" + tag.Status + ")", Type: security.UnauthorizedExit, Severity: "critical", Record: true}
	}
}
