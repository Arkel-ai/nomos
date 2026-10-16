package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var citationSourcesLog = u.NewLogger("services.citation_sources")

// Official revalidation.
//
// Every legal text Nomos stores is a CACHE: DILA is explicit that the
// opposable text is the authenticated Légifrance publication, and Judilibre
// is the Cour de cassation's own. Before a legal citation is published, the
// final gate fetches the exact version it names from the official source —
// Légifrance getArticle by LEGIARTI id, Judilibre decision by id — and
// requires the same identity, the same text and an applicability compatible
// with the period requested. Three outcomes, never collapsed:
//
//   - matched: the official round-trip happened and agreed. Only this
//     outcome lets a citation be labelled officially verified;
//   - mismatch: the official source answered something else — another
//     version, another text, no such version, not in force for the period.
//     The citation is refused. The version it names is never swapped for the
//     one the source returned: a dispute about 2012 cites the 2012 text;
//   - unavailable: the source could not be asked. Nothing is published; the
//     issue is retryable when the source may answer later.
//
// The resolver is injected (VerifierWith); nothing here is a package hook.

// OfficialSources is the official publication a stored legal version is
// revalidated against.
type OfficialSources interface {
	// Article returns the Légifrance article version with exactly this
	// LEGIARTI id, as published.
	Article(ctx context.Context, id string) (connectors.Article, error)
	// Decision returns the Judilibre decision with this id, as published.
	Decision(ctx context.Context, id string) (connectors.Decision, error)
}

// PisteSources is the production OfficialSources: Légifrance and Judilibre on
// PISTE through the process's shared clients, resolved at call time so the
// configuration loaded at boot is the one used.
func PisteSources() OfficialSources { return pisteSources{} }

// NewOfficialSources binds explicit connectors — a fake PISTE in a test.
func NewOfficialSources(lf *connectors.Legifrance, jl *connectors.Judilibre) OfficialSources {
	return pisteSources{lf: lf, jl: jl, bound: true}
}

type pisteSources struct {
	lf    *connectors.Legifrance
	jl    *connectors.Judilibre
	bound bool
}

func (s pisteSources) clients() (*connectors.Legifrance, *connectors.Judilibre) {
	if s.bound {
		return s.lf, s.jl
	}
	return pisteClients()
}

func (s pisteSources) Article(ctx context.Context, id string) (connectors.Article, error) {
	lf, _ := s.clients()
	if lf == nil {
		return connectors.Article{}, connectors.ErrLegifranceNotConfigured
	}
	return lf.GetArticle(ctx, id)
}

func (s pisteSources) Decision(ctx context.Context, id string) (connectors.Decision, error) {
	_, jl := s.clients()
	if jl == nil {
		return connectors.Decision{}, connectors.ErrJudilibreNotConfigured
	}
	return jl.Decision(ctx, id)
}

// Outcomes of an official revalidation.
const (
	OfficialMatched     = "matched"
	OfficialMismatch    = "mismatch"
	OfficialUnavailable = "unavailable"
)

// OfficialOutcome is one stored legal source checked against its official
// publication.
type OfficialOutcome struct {
	Status string // Official*
	// Retryable marks an unavailable source that may answer later (timeout,
	// 5xx, rate limit) — never a missing configuration or a refusal.
	Retryable bool
	// Correction is the server-written guidance for the agent. It never
	// quotes the source text.
	Correction string
	// OfficialID and TextSHA256 identify what the official source returned.
	OfficialID string
	TextSHA256 string
}

// officialCallTimeout bounds one official round-trip, connector retries
// included. A variable so tests can shorten it.
var officialCallTimeout = 45 * time.Second

const (
	officialNotConfigured = "Aucune source officielle n'est configurée sur ce serveur : cette citation juridique ne peut pas " +
		"être revérifiée et ne peut donc pas être publiée. Retirez-la, ou terminez avec une réponse qui indique cette limite."
	officialDown = "La source officielle n'a pas répondu (indisponibilité temporaire) : cette citation juridique n'a pas pu " +
		"être revérifiée et rien n'est publié. Soumettez de nouveau un peu plus tard, ou terminez avec une réponse qui ne " +
		"s'appuie pas sur elle et qui indique cette limite."
	officialRefusedRequest = "La source officielle refuse la requête : cette citation juridique ne peut pas être revérifiée. " +
		"Retirez-la, ou terminez avec une réponse qui indique cette limite."
	officialNotFound = "La source officielle ne connaît pas la version citée : elle ne peut pas être citée. Relisez la source " +
		"(read_law, read_decision) ou retirez la référence."
	officialOtherVersion = "La source officielle renvoie une autre version que celle citée : la version citée n'est jamais " +
		"remplacée par une autre. Relisez la version applicable à la période (search_law avec la date, puis read_law)."
	officialTextDiffers = "Le texte officiel de la version citée diffère du texte enregistré : la citation est refusée. " +
		"Relisez la source (read_law, read_decision) pour citer le texte officiel."
	officialNotInForce = "Selon la source officielle, cette version n'était pas en vigueur pendant la période applicable : " +
		"citez la version applicable (search_law avec la date), sans la remplacer par le texte actuel."
	officialNoIdentity = "Cette version n'a pas d'identifiant officiel exact : elle ne peut pas être revérifiée sur la source " +
		"officielle. Relisez la source avec read_law ou read_decision."
	officialDateDiffers = "La date de la décision officielle ne correspond pas à la décision enregistrée : relisez-la avec read_decision."
	officialAfterCutoff = "Selon la source officielle, cette décision est postérieure à la date limite de jurisprudence " +
		"demandée : retirez-la ou cherchez une décision antérieure."
)

// RevalidateOfficial fetches the exact version src names from its official
// source and compares identity, text and temporal applicability. It never
// writes: the stored version stays what it is, whatever the source says.
func RevalidateOfficial(ctx context.Context, sources OfficialSources, src AgentLegalSource, basis LegalTemporalBasis) OfficialOutcome {
	if sources == nil {
		return OfficialOutcome{Status: OfficialUnavailable, Correction: officialNotConfigured}
	}
	ctx, cancel := context.WithTimeout(ctx, officialCallTimeout)
	defer cancel()
	kind, out := "article", OfficialOutcome{}
	if src.Document.Kind == legalKindDecision || src.Document.Source == types.SourceJudilibre {
		kind, out = "decision", revalidateDecision(ctx, sources, src, basis)
	} else {
		out = revalidateArticle(ctx, sources, src, basis)
	}
	if out.Status == OfficialMatched {
		citationSourcesLog.Debugf("Official revalidation matched (kind=%s version=%s)", kind, src.Version.ID)
	} else {
		citationSourcesLog.Warnf("Official revalidation %s (kind=%s version=%s retryable=%t)", out.Status, kind, src.Version.ID, out.Retryable)
	}
	return out
}

func revalidateArticle(ctx context.Context, sources OfficialSources, src AgentLegalSource, basis LegalTemporalBasis) OfficialOutcome {
	id := officialArticleID(src.Version)
	if id == "" {
		return officialMismatch(officialNoIdentity, "")
	}
	a, err := sources.Article(ctx, id)
	if err != nil {
		return officialFailure(err)
	}
	if a.ID != id {
		// Another version came back (the current one, typically). It is
		// never taken in place of the version cited.
		return officialMismatch(officialOtherVersion, a.ID)
	}
	// Légifrance publishes the article twice: as plain text and as HTML.
	// The corpus loader stores the HTML rendering (the code tree's), read_law
	// by LEGIARTI id stores the plain one, and the two differ around
	// cross-reference links. Each stored text is compared with the official
	// rendering it was made from — never with a looser normalisation.
	stored := officialTextSHA256(src.Version.Text)
	sum := officialTextSHA256(a.Text)
	if sum != stored && strings.TrimSpace(a.TextHTML) != "" {
		sum = officialTextSHA256(a.HTMLText())
	}
	if sum != stored {
		return officialMismatch(officialTextDiffers, a.ID)
	}
	from, to := a.Validity()
	if types.NormalizeLegalVersionState(a.Etat) == types.LegalVersionAnnulled ||
		!versionApplicable(types.LegalVersion{ValidFrom: from, ValidTo: to}, basis) {
		return officialMismatch(officialNotInForce, a.ID)
	}
	return OfficialOutcome{Status: OfficialMatched, OfficialID: a.ID, TextSHA256: sum}
}

func revalidateDecision(ctx context.Context, sources OfficialSources, src AgentLegalSource, basis LegalTemporalBasis) OfficialOutcome {
	id := src.Document.SourceID
	if id == "" {
		return officialMismatch(officialNoIdentity, "")
	}
	d, err := sources.Decision(ctx, id)
	if err != nil {
		return officialFailure(err)
	}
	if d.ID != id {
		return officialMismatch(officialOtherVersion, d.ID)
	}
	decided := d.DecidedAt()
	if src.Version.DecisionDate != nil && (decided == nil || *decided != *src.Version.DecisionDate) {
		return officialMismatch(officialDateDiffers, d.ID)
	}
	sum := officialTextSHA256(d.Text)
	if sum != officialTextSHA256(src.Version.Text) {
		return officialMismatch(officialTextDiffers, d.ID)
	}
	if basis.CaseLawCutoff != nil && (decided == nil || *decided >= *basis.CaseLawCutoff) {
		return officialMismatch(officialAfterCutoff, d.ID)
	}
	return OfficialOutcome{Status: OfficialMatched, OfficialID: d.ID, TextSHA256: sum}
}

func officialMismatch(correction, officialID string) OfficialOutcome {
	return OfficialOutcome{Status: OfficialMismatch, Correction: correction, OfficialID: officialID}
}

// officialFailure classifies a failed official call.
func officialFailure(err error) OfficialOutcome {
	var api *connectors.APIError
	switch {
	case errors.Is(err, connectors.ErrLegifranceNotConfigured), errors.Is(err, connectors.ErrJudilibreNotConfigured):
		return OfficialOutcome{Status: OfficialUnavailable, Correction: officialNotConfigured}
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		return officialMismatch(officialNotFound, "")
	case errors.As(err, &api) && !api.Retryable:
		return OfficialOutcome{Status: OfficialUnavailable, Correction: officialRefusedRequest}
	case strings.HasSuffix(err.Error(), " not found"):
		// The connectors report an empty official answer as
		// "<source>: <kind> <id> not found".
		return officialMismatch(officialNotFound, "")
	default:
		return OfficialOutcome{Status: OfficialUnavailable, Retryable: true, Correction: officialDown}
	}
}

// officialTextSHA256 hashes a legal text with its whitespace runs normalised:
// the same rendering of an article differs only in line breaks and spacing
// between Légifrance and the stored copy. Every other character must be
// identical.
func officialTextSHA256(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

// officialCheckFor is the successful official revalidation a report records
// for one evidence span, or nil.
func officialCheckFor(r types.VerificationReport, evidenceID string) *types.OfficialCheck {
	for i := range r.Official {
		if r.Official[i].EvidenceID == evidenceID {
			return &r.Official[i]
		}
	}
	return nil
}

// isLegalSourceType reports whether a citation source type is a legal one,
// which publication requires to be officially revalidated.
func isLegalSourceType(t string) bool {
	return t == types.CitationSourceLegifrance || t == types.CitationSourceJudilibre
}

// errOfficialNotRevalidated is the publication refusal of a legal citation
// whose final report records no official revalidation.
var errOfficialNotRevalidated = fmt.Errorf("%w: a legal citation was not revalidated against the official source", ErrPublicationRefused)
