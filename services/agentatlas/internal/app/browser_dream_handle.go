package app

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
)

const (
	browserDreamHandleTTL     = 30 * time.Minute
	browserDreamHandlePrefix  = "agentatlas-dream-handle-v1:"
	maxBrowserDreamHandleSize = 4096
)

var errInvalidBrowserDreamHandle = errors.New("invalid browser Dream handle")

type browserDreamHandleClaim struct {
	Version, Kind, EnterpriseID, SessionBinding, OrgUnitID, ResourceID string
	ExpiresAt                                                          int64
}

type browserDreamHandleCodec struct {
	protector *browsersession.Protector
	now       func() time.Time
}

func newBrowserDreamHandleCodec(protector *browsersession.Protector, now func() time.Time) *browserDreamHandleCodec {
	if now == nil {
		now = time.Now
	}
	return &browserDreamHandleCodec{protector: protector, now: now}
}

func (c *browserDreamHandleCodec) issue(session browsersession.Session, kind, org, resourceID string) (string, error) {
	if c == nil || c.protector == nil || kind == "" || org == "" || resourceID == "" || session.EnterpriseID == "" || browserDreamSessionBinding(session) == "" {
		return "", errInvalidBrowserDreamHandle
	}
	claim := browserDreamHandleClaim{Version: "v1", Kind: kind, EnterpriseID: session.EnterpriseID, SessionBinding: browserDreamSessionBinding(session), OrgUnitID: org, ResourceID: resourceID, ExpiresAt: c.now().Add(browserDreamHandleTTL).Unix()}
	payload, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	sealed, err := c.protector.Seal(browserDreamHandlePrefix + string(payload))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *browserDreamHandleCodec) resolve(session browsersession.Session, kind, handle string) (browserDreamHandleClaim, error) {
	if c == nil || c.protector == nil || len(handle) < 32 || len(handle) > maxBrowserDreamHandleSize {
		return browserDreamHandleClaim{}, errInvalidBrowserDreamHandle
	}
	sealed, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil {
		return browserDreamHandleClaim{}, errInvalidBrowserDreamHandle
	}
	plain, err := c.protector.Open(sealed)
	if err != nil || !strings.HasPrefix(plain, browserDreamHandlePrefix) {
		return browserDreamHandleClaim{}, errInvalidBrowserDreamHandle
	}
	var claim browserDreamHandleClaim
	if json.Unmarshal([]byte(strings.TrimPrefix(plain, browserDreamHandlePrefix)), &claim) != nil || claim.Version != "v1" || claim.Kind != kind || claim.EnterpriseID != session.EnterpriseID || claim.SessionBinding != browserDreamSessionBinding(session) || claim.OrgUnitID == "" || claim.ResourceID == "" || claim.ExpiresAt <= c.now().Unix() {
		return browserDreamHandleClaim{}, errInvalidBrowserDreamHandle
	}
	return claim, nil
}

func browserDreamSessionBinding(session browsersession.Session) string {
	return session.FamilyID
}
