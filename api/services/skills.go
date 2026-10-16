package services

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var skillsLog = u.NewLogger("services.skills")

// Compétences — playbooks extracted from a model act (docs/PLAN.md §8).
// Skills are IMMUTABLE VERSIONS: `skills` holds identity and current_version,
// `skill_versions` holds the definition, the source asset, the reference docx
// and the extractor model. A mutable JSON blob cannot reproduce an earlier
// pleading, and reproducing an earlier pleading is the whole point. Scoped per
// tenant — one firm's skills never reach another.
//
// The consequence, everywhere below: NOTHING updates a skill_versions row.
// Ever. A change to a compétence is an INSERT of version N+1 plus a move of
// the `skills.current_version` pointer; version N stays byte-for-byte what the
// pleading that used it was built from. skills_test.go is the proof.

// SkillSchemaVersion pins the shape of the definition JSON. A definition
// written under an older schema is still readable — that is why the version
// travels on the row rather than in a global constant the reader has to guess.
const SkillSchemaVersion = 1

// skillExtractorBuiltin is the "extractor model" recorded for the definitions
// that ship with Nomos. They were written by hand, not extracted, and the
// audit trail must say so rather than name a model that never saw them.
const skillExtractorBuiltin = "builtin"

// skillMaxTextRunes bounds what a model act sends to the gateway. A playbook
// lives in the structure of an act — its sections, its formulas, its closing
// clauses — not in its eightieth page of annexes.
const skillMaxTextRunes = 30000

// skillSlugPattern is the slash trigger, and therefore a closed alphabet: it
// ends up after a "/" in the composer.
var skillSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)

// ---------------------------------------------------------------------------
// The definition
// ---------------------------------------------------------------------------

// skillDefinition is the SERVER-OWNED shape of a compétence: what the act is
// made of, how it is written, what it must never omit, and what a human checks
// before it leaves the cabinet.
type skillDefinition struct {
	Sections         []skillSection `json:"sections"`
	Tone             skillTone      `json:"tone"`
	MandatoryClauses []skillClause  `json:"mandatory_clauses"`
	Checklist        []skillCheck   `json:"checklist"`
}

type skillSection struct {
	Title    string `json:"title"`
	Purpose  string `json:"purpose"`
	Required bool   `json:"required"`
}

type skillTone struct {
	Register string `json:"register"` // soutenu | standard | direct
	Person   string `json:"person"`   // e.g. "3e personne", "1re personne du pluriel"
	Notes    string `json:"notes"`
}

type skillClause struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	Why   string `json:"why"`
}

type skillCheck struct {
	Item     string `json:"item"`
	Blocking bool   `json:"blocking"`
}

// skillSchema is the JSON schema sent as response_format AND re-validated
// locally by utils.ChatJSON (§4.3). Every property is required and
// additionalProperties is false: a playbook with a silently missing checklist
// is worse than an extraction that failed loudly.
func skillSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"sections", "tone", "mandatory_clauses", "checklist"},
		"properties": map[string]any{
			"sections": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"title", "purpose", "required"},
					"properties": map[string]any{
						"title":    map[string]any{"type": "string"},
						"purpose":  map[string]any{"type": "string"},
						"required": map[string]any{"type": "boolean"},
					},
				},
			},
			"tone": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"register", "person", "notes"},
				"properties": map[string]any{
					"register": map[string]any{"enum": []any{"soutenu", "standard", "direct"}},
					"person":   map[string]any{"type": "string"},
					"notes":    map[string]any{"type": "string"},
				},
			},
			"mandatory_clauses": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"label", "text", "why"},
					"properties": map[string]any{
						"label": map[string]any{"type": "string"},
						"text":  map[string]any{"type": "string"},
						"why":   map[string]any{"type": "string"},
					},
				},
			},
			"checklist": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"item", "blocking"},
					"properties": map[string]any{
						"item":     map[string]any{"type": "string"},
						"blocking": map[string]any{"type": "boolean"},
					},
				},
			},
		},
	}
}

// skillSystemPrompt is STATIC: the identity and the task, never
// the act being read. Every variable byte travels in the user message.
const skillSystemPrompt = `<role>
Tu es un assistant juridique français. Tu lis un ACTE MODÈLE fourni par un
cabinet d'avocats et tu en extrais la TRAME réutilisable : la structure de
l'acte, le ton de rédaction du cabinet, les clauses qui ne peuvent jamais
manquer, et les points qu'un avocat doit vérifier avant signature.
Tu ne donnes aucun conseil juridique et tu n'inventes jamais une clause que
l'acte ne contient pas.
</role>

<tasks>
1. Relever les SECTIONS de l'acte, dans l'ordre où elles apparaissent
   (sections) : pour chacune, un titre court, l'objet de la section en une
   phrase, et si elle est indispensable à ce type d'acte (required).
2. Décrire le TON de rédaction (tone) : le registre ("soutenu", "standard" ou
   "direct"), la personne employée (par exemple "3e personne" ou
   "1re personne du pluriel"), et les habitudes de rédaction remarquables
   (formules d'introduction, numérotation, usage des intertitres).
3. Relever les CLAUSES OBLIGATOIRES (mandatory_clauses) : les mentions dont
   l'absence rendrait l'acte irrégulier ou incomplet. Pour chacune, un
   intitulé, la formule telle qu'elle figure dans l'acte modèle, et la raison
   pour laquelle elle est nécessaire. N'invente aucune clause.
4. Établir la LISTE DE CONTRÔLE (checklist) qu'un avocat parcourt avant
   d'envoyer l'acte : dates, montants, parties, prétentions, pièces visées.
   Marque blocking = true lorsqu'un manquement interdit l'envoi.
</tasks>

<output>
Un unique document JSON, conforme exactement au schéma imposé, sans texte
avant ni après, sans bloc de code, sans commentaire.
</output>`

// skillUserPrompt carries every variable byte. Nothing here is ever logged
// (§7.4).
func skillUserPrompt(name, description, text string) string {
	runes := []rune(text)
	truncated := false
	if len(runes) > skillMaxTextRunes {
		runes = runes[:skillMaxTextRunes]
		truncated = true
	}
	var b strings.Builder
	b.WriteString("<competence>\n<nom>")
	b.WriteString(name)
	b.WriteString("</nom>\n")
	if description != "" {
		b.WriteString("<description>")
		b.WriteString(description)
		b.WriteString("</description>\n")
	}
	b.WriteString("<acte_modele>\n")
	b.WriteString(string(runes))
	b.WriteString("\n</acte_modele>\n")
	if truncated {
		b.WriteString("<note>L'acte a été tronqué : seul son début est fourni.</note>\n")
	}
	b.WriteString("</competence>")
	return b.String()
}

// ---------------------------------------------------------------------------
// Built-in compétences — the slash commands the composer offers (§11)
// ---------------------------------------------------------------------------

type builtinSkill struct {
	Slug        string
	Name        string
	Description string
	Definition  skillDefinition
}

// builtinSkills are seeded per tenant so that a built-in and a firm's own
// compétence are the SAME kind of object: a versioned row with an immutable
// definition. A built-in that lived only in Go would be a second, invisible
// code path the reproducibility guarantee does not cover.
func builtinSkills() []builtinSkill {
	return []builtinSkill{
		{
			Slug:        "note-de-synthese",
			Name:        "Note de synthèse",
			Description: "Faits, prétentions, moyens et pièces qui les soutiennent.",
			Definition: skillDefinition{
				Sections: []skillSection{
					{Title: "Objet de la note", Purpose: "La question posée et le périmètre traité.", Required: true},
					{Title: "Rappel des faits", Purpose: "Les faits utiles, datés, chacun rattaché à la pièce qui l'établit.", Required: true},
					{Title: "Procédure", Purpose: "Les actes de procédure déjà accomplis et le stade atteint.", Required: false},
					{Title: "Prétentions des parties", Purpose: "Ce que chaque partie demande et sur quel fondement.", Required: true},
					{Title: "Discussion", Purpose: "Les moyens, texte par texte, avec la jurisprudence applicable à la date des faits.", Required: true},
					{Title: "Appréciation du risque", Purpose: "Les points forts, les points faibles et les zones d'incertitude.", Required: true},
					{Title: "Pièces citées", Purpose: "La liste des pièces visées, dans l'ordre du bordereau.", Required: true},
				},
				Tone: skillTone{Register: "soutenu", Person: "3e personne", Notes: "Style impersonnel, phrases courtes, aucune affirmation sans pièce ou source."},
				MandatoryClauses: []skillClause{
					{Label: "Réserve de relecture", Text: "La présente note est un projet soumis à la vérification de l'avocat.", Why: "Aucun livrable non approuvé ne quitte le cabinet sans cette mention (§13)."},
				},
				Checklist: []skillCheck{
					{Item: "Chaque fait daté renvoie à une pièce du bordereau.", Blocking: true},
					{Item: "Chaque texte cité est applicable à la date des faits.", Blocking: true},
					{Item: "Aucune décision n'est présentée comme un revirement sans vérification.", Blocking: true},
					{Item: "Les prétentions chiffrées sont recalculées.", Blocking: false},
				},
			},
		},
		{
			Slug:        "mise-en-demeure",
			Name:        "Mise en demeure",
			Description: "Sommation préalable, obligation, délai et conséquences.",
			Definition: skillDefinition{
				Sections: []skillSection{
					{Title: "Identification des parties", Purpose: "L'expéditeur, le destinataire et la qualité de chacun.", Required: true},
					{Title: "Rappel de la relation", Purpose: "Le contrat ou le fait générateur de l'obligation.", Required: true},
					{Title: "Manquement reproché", Purpose: "Ce qui n'a pas été exécuté, daté et documenté.", Required: true},
					{Title: "Obligation dont l'exécution est réclamée", Purpose: "Ce qui est exigé, précisément et chiffré.", Required: true},
					{Title: "Délai imparti", Purpose: "Le délai d'exécution, à compter de la réception.", Required: true},
					{Title: "Conséquences du défaut", Purpose: "Les suites judiciaires envisagées à défaut d'exécution.", Required: true},
				},
				Tone: skillTone{Register: "soutenu", Person: "1re personne du pluriel", Notes: "Ferme et factuel, sans invective ; toute somme réclamée est justifiée."},
				MandatoryClauses: []skillClause{
					{Label: "Mise en demeure expresse", Text: "La présente vaut mise en demeure au sens de l'article 1344 du code civil.", Why: "Sans mention expresse, la sommation ne fait pas courir les intérêts moratoires."},
					{Label: "Réserve de droits", Text: "Le tout sous toutes réserves, notamment de droits, moyens et actions.", Why: "Préserve les demandes non formulées dans le courrier."},
				},
				Checklist: []skillCheck{
					{Item: "Le destinataire et sa forme sociale sont exacts.", Blocking: true},
					{Item: "Le délai imparti est déterminé et raisonnable.", Blocking: true},
					{Item: "Les sommes réclamées sont détaillées poste par poste.", Blocking: true},
					{Item: "Le mode d'envoi permet de prouver la réception.", Blocking: false},
				},
			},
		},
		{
			Slug:        "assignation",
			Name:        "Assignation",
			Description: "Acte introductif d'instance : parties, faits, discussion, dispositif.",
			Definition: skillDefinition{
				Sections: []skillSection{
					{Title: "Désignation des parties", Purpose: "Demandeur, défendeur et leurs représentants.", Required: true},
					{Title: "Juridiction saisie", Purpose: "La juridiction, sa compétence et les date et heure d'audience.", Required: true},
					{Title: "Exposé des faits", Purpose: "Les faits datés, rattachés aux pièces communiquées.", Required: true},
					{Title: "Discussion", Purpose: "Les moyens en droit, fondement par fondement.", Required: true},
					{Title: "Dispositif", Purpose: "Les demandes, formulées en « PAR CES MOTIFS ».", Required: true},
					{Title: "Bordereau de pièces", Purpose: "Les pièces communiquées, numérotées.", Required: true},
				},
				Tone: skillTone{Register: "soutenu", Person: "3e personne", Notes: "Structure numérotée, un moyen par sous-titre, dispositif en majuscules."},
				MandatoryClauses: []skillClause{
					{Label: "Mentions de l'article 56 CPC", Text: "L'objet de la demande, l'exposé des moyens, l'indication des pièces et les diligences en vue d'une résolution amiable.", Why: "Leur absence est sanctionnée par la nullité de l'acte."},
					{Label: "Constitution d'avocat", Text: "La constitution de l'avocat du demandeur et le délai pour constituer.", Why: "Mention obligatoire devant les juridictions à représentation obligatoire."},
				},
				Checklist: []skillCheck{
					{Item: "Les mentions obligatoires de l'assignation sont toutes présentes.", Blocking: true},
					{Item: "Le dispositif reprend chaque demande discutée, et rien d'autre.", Blocking: true},
					{Item: "Le bordereau correspond exactement aux pièces visées.", Blocking: true},
					{Item: "La date d'audience et le délai de comparution sont vérifiés.", Blocking: true},
				},
			},
		},
		{
			Slug:        "chronologie",
			Name:        "Chronologie",
			Description: "Frise datée des faits et des actes, chaque date adossée à une pièce.",
			Definition: skillDefinition{
				Sections: []skillSection{
					{Title: "Période couverte", Purpose: "La première et la dernière date retenues.", Required: true},
					{Title: "Chronologie", Purpose: "Une ligne par événement : date, événement, pièce.", Required: true},
					{Title: "Dates incertaines", Purpose: "Les événements dont la date n'est pas établie, et pourquoi.", Required: true},
					{Title: "Manques", Purpose: "Les périodes sans pièce, à instruire.", Required: false},
				},
				Tone: skillTone{Register: "standard", Person: "3e personne", Notes: "Une phrase par événement, aucune interprétation, aucune date reconstituée."},
				MandatoryClauses: []skillClause{
					{Label: "Traçabilité", Text: "Chaque date est suivie du numéro de la pièce qui l'établit.", Why: "Une chronologie sans pièce n'est pas opposable."},
				},
				Checklist: []skillCheck{
					{Item: "Aucune date n'est déduite sans pièce.", Blocking: true},
					{Item: "Les dates partielles sont indiquées comme telles (mois ou année).", Blocking: true},
					{Item: "L'ordre chronologique est strict.", Blocking: false},
				},
			},
		},
		{
			Slug:        "risques-contractuels",
			Name:        "Risques contractuels",
			Description: "Analyse clause par clause, avec citation exacte de la clause.",
			Definition: skillDefinition{
				Sections: []skillSection{
					{Title: "Contrats analysés", Purpose: "Les contrats et avenants examinés, avec leur date.", Required: true},
					{Title: "Risques identifiés", Purpose: "Un bloc par risque : clause citée, analyse, gravité.", Required: true},
					{Title: "Clauses manquantes", Purpose: "Les stipulations attendues et absentes.", Required: true},
					{Title: "Recommandations", Purpose: "Les modifications proposées, par ordre de priorité.", Required: false},
				},
				Tone: skillTone{Register: "standard", Person: "3e personne", Notes: "Chaque risque cite la clause entre guillemets avant de l'analyser."},
				MandatoryClauses: []skillClause{
					{Label: "Citation littérale", Text: "La clause commentée est reproduite mot pour mot avant analyse.", Why: "Une analyse sans le texte de la clause n'est pas vérifiable."},
				},
				Checklist: []skillCheck{
					{Item: "Chaque risque cite la clause exacte et sa localisation.", Blocking: true},
					{Item: "La version du contrat analysée est celle applicable aux faits.", Blocking: true},
					{Item: "La gravité annoncée est justifiée.", Blocking: false},
				},
			},
		},
		// A DRAFT awaiting the owner's validation as a lawyer (owner decision
		// 5): the description says so in the `/` menu and in list_competences.
		// Every reference below was read on PISTE (Légifrance, Judilibre) on
		// 15 Oct 2026. The gate matches section titles against headings and
		// clause texts literally.
		{
			Slug:        "requete-cph-resiliation-judiciaire",
			Name:        "Requête CPH — résiliation judiciaire du contrat de travail",
			Description: "Projet à valider par l'avocat avant usage. Requête saisissant le conseil de prud'hommes d'une demande de résiliation judiciaire aux torts de l'employeur.",
			Definition: skillDefinition{
				Sections: []skillSection{
					{Title: "Juridiction saisie", Purpose: "Le conseil de prud'hommes saisi et la section compétente. Mention prescrite à peine de nullité par l'article 54, 1°, du code de procédure civile, applicable à la requête par renvoi des articles 57 du même code et R. 1452-2 du code du travail. Compétence territoriale : article R. 1412-1 du code du travail ; répartition entre sections : articles L. 1423-1-1 et L. 1423-1-2 du même code. À demander à l'avocat s'ils ne ressortent pas des pièces.", Required: true},
					{Title: "Parties", Purpose: "Le demandeur : nom, prénoms, profession, domicile, nationalité, date et lieu de naissance (article 54, 3°, a, du code de procédure civile), et l'avocat qui l'assiste ou le représente. Le défendeur : pour une personne morale, sa dénomination et son siège social ; pour une personne physique, ses nom, prénoms et domicile (article 57 du code de procédure civile) ; sa forme sociale et son représentant légal lorsqu'ils sont établis. Chaque élément vient d'une pièce ou de l'avocat ; un élément manquant est demandé, jamais complété.", Required: true},
					{Title: "Objet de la demande", Purpose: "En une ou deux phrases : la résiliation judiciaire du contrat de travail aux torts de l'employeur et les effets demandés, tels que l'avocat les a arrêtés. Mention prescrite à peine de nullité par l'article 54, 2°, du code de procédure civile.", Required: true},
					{Title: "Exposé des faits", Purpose: "L'exposé sommaire des motifs de la demande exigé par l'article R. 1452-2 du code du travail. D'abord la relation de travail : date d'engagement, emploi, classification, rémunération, convention collective applicable. Puis les faits reprochés à l'employeur, datés, dans l'ordre chronologique, chacun suivi du numéro de la pièce qui l'établit. Chaque fait est lu dans la pièce elle-même, jamais déduit de son titre ou de sa qualification ; un fait que les pièces n'établissent pas est signalé comme tel.", Required: true},
					{Title: "Moyens", Purpose: "Un sous-titre par manquement de l'employeur : l'obligation méconnue, les faits et pièces qui l'établissent, et en quoi le manquement est suffisamment grave pour empêcher la poursuite du contrat de travail. Fondements à lire avant toute citation : la résolution judiciaire pour inexécution suffisamment grave (articles 1224, 1227 et 1228 du code civil, dans leur rédaction en vigueur depuis le 1er octobre 2016 ; pour un contrat conclu avant cette date, rechercher la rédaction applicable, l'ancien article 1184 du code civil ayant fondé la résiliation judiciaire) et l'article L. 1231-1 du code du travail. Jurisprudence à relire : un manquement rendant impossible la poursuite du contrat de travail justifie la résiliation (Cass. soc., 26 mars 2014, n° 12-21.372, publié au Bulletin) ; un changement qui n'est pas de nature à empêcher cette poursuite ne la justifie pas (Cass. soc., 12 juin 2014, n° 13-11.448, publié au Bulletin). Puis les effets de la résiliation et la justification de chaque chef de demande, avec son calcul : article L. 1235-3 du code du travail lorsque la résiliation produit les effets d'un licenciement sans cause réelle et sérieuse et prend effet après la publication de l'ordonnance n° 2017-1387 du 22 septembre 2017 (Cass. soc., 16 février 2022, n° 20-16.184, publié au Bulletin) ; article L. 1235-3-1 du même code lorsque l'avocat retient une nullité.", Required: true},
					{Title: "Par ces motifs", Purpose: "Chacun des chefs de la demande, mentionné un par un comme l'exige l'article R. 1452-2 du code du travail, et chiffré lorsqu'il porte sur une somme : la résiliation judiciaire aux torts de l'employeur et ses effets, puis les indemnités et dommages et intérêts, les rappels de salaire, la remise des documents de fin de contrat, les intérêts au taux légal, l'exécution provisoire (sauf disposition contraire, les décisions du conseil de prud'hommes ne sont pas exécutoires de droit à titre provisoire : article R. 1454-28 du code du travail), les frais non compris dans les dépens (article 700 du code de procédure civile) et les dépens. Ne figurent que les chefs arrêtés par l'avocat et discutés dans les moyens ; aucun montant n'est inventé.", Required: true},
					{Title: "Bordereau de pièces", Purpose: "Le bordereau annexé à la requête, énumérant les pièces que le demandeur invoque à l'appui de ses prétentions (article R. 1452-2 du code du travail ; l'indication des pièces est exigée à peine de nullité par l'article 57 du code de procédure civile), numérotées dans l'ordre où l'exposé les cite. Pour une instance introduite à compter du 1er octobre 2026, la requête est en outre accompagnée du dernier bulletin de salaire afférent au litige ou de toute pièce permettant de déterminer l'activité de l'employeur (article R. 1452-2, dernier alinéa, du code du travail).", Required: true},
				},
				Tone: skillTone{Register: "soutenu", Person: "3e personne", Notes: "Style de requête : le salarié est désigné par son nom ou comme « le demandeur », l'employeur par sa dénomination. Faits au passé, datés, chacun suivi du numéro de la pièce qui l'établit ; un manquement par sous-titre ; les chefs de demande en liste, chacun introduit par un verbe à l'infinitif en majuscules (PRONONCER, CONDAMNER, ORDONNER). Aucune affirmation sans pièce ou source."},
				MandatoryClauses: []skillClause{
					{Label: "Chef de demande principal", Text: "Prononcer la résiliation judiciaire du contrat de travail", Why: "L'objet de la demande et chacun de ses chefs figurent dans la requête (article 54, 2°, du code de procédure civile ; article R. 1452-2 du code du travail)."},
					{Label: "Date et signature", Text: "Fait à", Why: "La requête est datée et signée (article 57 du code de procédure civile) : « Fait à [lieu], le [date] », puis la signature de l'avocat."},
				},
				Checklist: []skillCheck{
					{Item: "Identité complète du demandeur (nom, prénoms, profession, domicile, nationalité, date et lieu de naissance) : tirée des pièces ou demandée à l'avocat, jamais complétée.", Blocking: true},
					{Item: "Désignation du défendeur (dénomination et siège social, ou nom, prénoms et domicile ; forme sociale et représentant légal) : tirée des pièces ou demandée à l'avocat.", Blocking: true},
					{Item: "Conseil de prud'hommes et section saisis : confirmés par l'avocat.", Blocking: true},
					{Item: "Objet et effets demandés de la résiliation (licenciement sans cause réelle et sérieuse, ou nul si l'avocat le retient) : arrêtés par l'avocat.", Blocking: true},
					{Item: "Chefs de demande chiffrés : chaque montant a sa base de calcul (salaire de référence, ancienneté, effectif de l'entreprise) tirée des pièces ou donnée par l'avocat ; aucun montant n'est inventé.", Blocking: true},
					{Item: "Pièces produites : liste numérotée arrêtée avec l'avocat, comprenant le dernier bulletin de salaire afférent au litige ou une pièce permettant de déterminer l'activité de l'employeur.", Blocking: true},
					{Item: "Contrat de travail en cours à la date de la requête : toute rupture déjà intervenue (licenciement, démission, prise d'acte, rupture conventionnelle) est signalée à l'avocat avant de rédiger.", Blocking: true},
					{Item: "Chaque fait de l'exposé renvoie à la pièce qui l'établit ; aucun fait n'est tiré des seules métadonnées d'une pièce.", Blocking: true},
					{Item: "Chaque manquement invoqué est daté, établi par une pièce, et sa gravité est discutée.", Blocking: true},
					{Item: "Chaque chef du « Par ces motifs » est discuté dans les moyens, et chaque montant est recalculé.", Blocking: true},
					{Item: "La prescription de chaque chef de demande est vérifiée (articles L. 1471-1 et L. 3245-1 du code du travail).", Blocking: true},
					{Item: "Chaque texte cité est lu dans la rédaction applicable aux faits et à l'instance, notamment pour un contrat conclu avant le 1er octobre 2016 ou une instance introduite avant le 1er octobre 2026.", Blocking: true},
					{Item: "Le bordereau correspond exactement aux pièces citées, dans le même ordre.", Blocking: true},
					{Item: "La requête est datée et signée.", Blocking: true},
					{Item: "Diligences de résolution amiable (article 54, 5°, du code de procédure civile) : mentionnées seulement si une tentative préalable était requise, sur confirmation de l'avocat.", Blocking: false},
					{Item: "La requête et le bordereau sont établis en autant d'exemplaires qu'il existe de défendeurs, outre l'exemplaire destiné à la juridiction (article R. 1452-2 du code du travail).", Blocking: false},
				},
			},
		},
	}
}

// ensureBuiltinSkills seeds the built-in compétences for a tenant, once. It is
// idempotent by construction: the skill insert is ON CONFLICT DO NOTHING on
// (tenant_id, slug) and the version insert on (skill_id, version), so two
// replicas racing on a first page load converge instead of duplicating.
//
// It NEVER rewrites an existing definition. If a built-in playbook changes, it
// ships as version N+1 through the same path as anything else.
func ensureBuiltinSkills(repo *u.TenantRepo) error {
	builtins := builtinSkills()
	slugs := make([]string, 0, len(builtins))
	for _, b := range builtins {
		slugs = append(slugs, b.Slug)
	}
	var present int64
	if err := repo.Skills().Where("slug IN ?", slugs).Count(&present).Error; err != nil {
		return err
	}
	if int(present) == len(builtins) {
		return nil
	}

	seeded := 0
	for _, b := range builtins {
		var skill types.Skill
		err := repo.Skills().Where("slug = ?", b.Slug).Take(&skill).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			skill = types.Skill{
				ID:             uuid.NewString(),
				Slug:           b.Slug,
				Name:           b.Name,
				Description:    b.Description,
				State:          types.SkillStateActive,
				CurrentVersion: 1,
				CreatedBy:      skillExtractorBuiltin,
			}
			if createErr := repo.Create(&skill); createErr != nil {
				// A racing replica won the unique index on (tenant, slug):
				// its row is the one the version must point at.
				if err = repo.Skills().Where("slug = ?", b.Slug).Take(&skill).Error; err != nil {
					return createErr
				}
			} else {
				seeded++
			}
		} else if err != nil {
			return err
		}

		var versions int64
		if err := repo.SkillVersions().
			Where("skill_id = ? AND version = ?", skill.ID, 1).Count(&versions).Error; err != nil {
			return err
		}
		if versions > 0 {
			continue
		}
		definition, err := json.Marshal(b.Definition)
		if err != nil {
			return err
		}
		version := types.SkillVersion{
			ID:             uuid.NewString(),
			SkillID:        skill.ID,
			Version:        1,
			SchemaVersion:  SkillSchemaVersion,
			Definition:     types.JSONB(definition),
			ExtractorModel: skillExtractorBuiltin,
			CreatedBy:      skillExtractorBuiltin,
		}
		if err := repo.Create(&version); err != nil {
			// Same race, on (skill_id, version). The definition already
			// exists and is immutable, so there is nothing to reconcile.
			skillsLog.Debugf("Built-in version insert skipped (tenant=%s slug=%s): %v",
				repo.TenantID(), b.Slug, err)
		}
	}
	if seeded > 0 {
		skillsLog.Infof("Built-in compétences seeded (tenant=%s count=%d)", repo.TenantID(), seeded)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

// ListSkills returns the tenant's compétences, paginated with a Count total.
func ListSkills(c *gin.Context, q types.ListQuery) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	if err := ensureBuiltinSkills(repo); err != nil {
		skillsLog.Errorf("Seeding built-in compétences failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	limit, offset := q.Normalized()

	var total int64
	if err := repo.Skills().Where("state <> ?", types.SkillStateArchived).Count(&total).Error; err != nil {
		skillsLog.Errorf("Skill count failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	rows := []types.Skill{}
	if err := repo.Skills().Where("state <> ?", types.SkillStateArchived).
		Order("name ASC").Limit(limit).Offset(offset).
		Find(&rows).Error; err != nil {
		skillsLog.Errorf("Skill select failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}

	skillsLog.Debugf("Listed compétences (tenant=%s rows=%d total=%d)", repo.TenantID(), len(rows), total)
	return http.StatusOK, types.ListResponse{Items: rows, Total: total, Limit: limit, Offset: offset}
}

// CreateSkill publishes a compétence version.
//
// It is deliberately NOT "create or update": posting a slug that already
// exists appends version N+1 and moves the pointer. The previous version row
// is left untouched, because an act drafted last March must still be
// reproducible from the definition that produced it (§8, §9.1(4)).
//
// The definition comes either from an uploaded model act — extracted against
// the server-owned schema above, with the extractor model recorded on the
// version — or from an explicit definition body.
func CreateSkill(c *gin.Context, body types.SkillCreateRequest) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c) // never nil: RequireAuth ran first

	slug := strings.ToLower(strings.TrimSpace(body.Slug))
	name := strings.TrimSpace(body.Name)
	if !skillSlugPattern.MatchString(slug) {
		return http.StatusBadRequest, gin.H{"detail": "Slug must be lowercase letters, digits and hyphens"}
	}
	if name == "" {
		return http.StatusBadRequest, gin.H{"detail": "Name is required"}
	}

	definition := types.JSONB(nil)
	extractorModel := ""
	switch {
	case len(body.Definition) > 0 && !bytesAreJSONNull(body.Definition):
		var parsed skillDefinition
		if err := json.Unmarshal(body.Definition, &parsed); err != nil {
			return http.StatusBadRequest, gin.H{"detail": "Definition does not match the compétence schema"}
		}
		encoded, err := json.Marshal(parsed)
		if err != nil {
			return http.StatusBadRequest, gin.H{"detail": "Definition could not be encoded"}
		}
		definition = types.JSONB(encoded)
		extractorModel = "manual"
	case body.SourceAssetID != nil && *body.SourceAssetID != "":
		encoded, model, status, errBody := extractSkillDefinition(c, repo, *body.SourceAssetID, name, strings.TrimSpace(body.Description))
		if encoded == nil {
			return status, errBody
		}
		definition, extractorModel = encoded, model
	default:
		return http.StatusBadRequest, gin.H{"detail": "A compétence needs a model act or an explicit definition"}
	}

	var detail types.SkillDetail
	err := repo.Tx(func(tx *u.TenantRepo) error {
		var skill types.Skill
		err := tx.Skills().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("slug = ?", slug).Take(&skill).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			skill = types.Skill{
				ID:          uuid.NewString(),
				Slug:        slug,
				Name:        name,
				Description: strings.TrimSpace(body.Description),
				State:       types.SkillStateActive,
				CreatedBy:   user.ID,
			}
			if err := tx.Create(&skill); err != nil {
				return err
			}
		case err != nil:
			return err
		}

		version := types.SkillVersion{
			ID:               uuid.NewString(),
			SkillID:          skill.ID,
			Version:          skill.CurrentVersion + 1,
			SourceAssetID:    body.SourceAssetID,
			ReferenceAssetID: body.ReferenceAssetID,
			SchemaVersion:    SkillSchemaVersion,
			Definition:       definition,
			ExtractorModel:   extractorModel,
			CreatedBy:        user.ID,
		}
		if err := tx.Create(&version); err != nil {
			return err
		}

		// The pointer move, and nothing else. No previously published row is
		// written to, here or anywhere.
		if err := tx.Skills().Where("id = ?", skill.ID).Updates(map[string]any{
			"current_version": version.Version,
			"name":            name,
			"description":     strings.TrimSpace(body.Description),
			"state":           types.SkillStateActive,
		}).Error; err != nil {
			return err
		}

		skill.CurrentVersion = version.Version
		skill.Name = name
		skill.Description = strings.TrimSpace(body.Description)
		skill.State = types.SkillStateActive
		detail = types.SkillDetail{Skill: skill}
		return tx.SkillVersions().Where("skill_id = ?", skill.ID).
			Order("version DESC").Find(&detail.Versions).Error
	})
	if err != nil {
		skillsLog.Errorf("Publishing compétence version failed (tenant=%s slug=%s): %v", repo.TenantID(), slug, err)
		return http.StatusBadGateway, gin.H{"detail": "Database insert failed"}
	}

	audit(repo, user.ID, "skill.publish", "skill", detail.ID, jsonbOf(map[string]any{
		"slug": slug, "version": detail.CurrentVersion, "extractor_model": extractorModel,
	}))
	skillsLog.Infof("Compétence published (tenant=%s slug=%s version=%d extractor=%s)",
		repo.TenantID(), slug, detail.CurrentVersion, extractorModel)
	return http.StatusCreated, detail
}

// GetSkill returns a compétence with its full version history — the history is
// the audit trail, so it is never collapsed to the current version.
func GetSkill(c *gin.Context, id string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	var skill types.Skill
	if err := repo.Skills().Where("id = ?", id).Take(&skill).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return http.StatusNotFound, gin.H{"detail": "Skill not found"}
		}
		skillsLog.Errorf("Skill lookup failed (skill=%s): %v", id, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	detail := types.SkillDetail{Skill: skill}
	if err := repo.SkillVersions().Where("skill_id = ?", skill.ID).
		Order("version DESC").Find(&detail.Versions).Error; err != nil {
		skillsLog.Errorf("Skill version select failed (skill=%s): %v", id, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	return http.StatusOK, detail
}

// DeleteSkill archives a compétence. Versions are retained: deleting them
// would make every pleading that used one unreproducible.
func DeleteSkill(c *gin.Context, id string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c)

	res := repo.Skills().Where("id = ?", id).Update("state", types.SkillStateArchived)
	if res.Error != nil {
		skillsLog.Errorf("Archiving compétence failed (skill=%s): %v", id, res.Error)
		return http.StatusBadGateway, gin.H{"detail": "Database update failed"}
	}
	if res.RowsAffected == 0 {
		return http.StatusNotFound, gin.H{"detail": "Skill not found"}
	}

	audit(repo, user.ID, "skill.archive", "skill", id, nil)
	skillsLog.Infof("Compétence archived (tenant=%s skill=%s) — versions retained", repo.TenantID(), id)
	return http.StatusOK, gin.H{"id": id, "state": types.SkillStateArchived}
}

// ---------------------------------------------------------------------------
// Extraction from a model act
// ---------------------------------------------------------------------------

// extractSkillDefinition reads the tenant's own asset, extracts its text with
// the same extractors ingestion uses, and asks the extraction model for the
// playbook. It returns (nil, "", status, body) on failure.
func extractSkillDefinition(c *gin.Context, repo *u.TenantRepo, assetID, name, description string) (types.JSONB, string, int, any) {
	var asset types.Asset
	if err := repo.Assets().Where("id = ?", assetID).Take(&asset).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", http.StatusNotFound, gin.H{"detail": "Model act not found"}
		}
		skillsLog.Errorf("Model act lookup failed (asset=%s): %v", assetID, err)
		return nil, "", http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}

	ctx := c.Request.Context()
	raw, err := ingestFetch(ctx, asset.S3Key)
	if err != nil {
		skillsLog.Errorf("Model act fetch failed (asset=%s): %v", assetID, err)
		return nil, "", http.StatusBadGateway, gin.H{"detail": "Model act could not be read"}
	}

	var pages []ingestPage
	switch asset.Mime {
	case "application/pdf":
		pages, err = pdfTextPages(ctx, raw)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		pages, err = docxTextPages(raw)
	default:
		return nil, "", http.StatusBadRequest, gin.H{"detail": "A model act must be a PDF or a .docx"}
	}
	if err != nil {
		skillsLog.Errorf("Model act text extraction failed (asset=%s mime=%s): %v", assetID, asset.Mime, err)
		return nil, "", http.StatusBadGateway, gin.H{"detail": "The model act text could not be extracted"}
	}
	text := strings.TrimSpace(ingestJoinPages(pages))
	if text == "" {
		return nil, "", http.StatusBadRequest, gin.H{"detail": "The model act has no text layer"}
	}

	cfg := config.Load()
	skillsLog.Debugf("Extracting compétence (tenant=%s asset=%s model=%s text_chars=%d)",
		repo.TenantID(), assetID, cfg.ExtractionModel, len([]rune(text)))

	// Temperature 0: two extractions of the same model act must not disagree
	// about what the firm's trame is.
	zero := 0.0
	var answer skillDefinition
	if err := u.ChatJSON(ctx, u.ChatOpts{
		Model:       cfg.ExtractionModel,
		System:      skillSystemPrompt,
		User:        skillUserPrompt(name, description, text),
		Temperature: &zero,
		MaxTokens:   4000,
		Purpose:     types.LLMPurposeQualify,
		TenantID:    repo.TenantID(),
	}, skillSchema(), &answer); err != nil {
		skillsLog.Errorf("Compétence extraction failed (tenant=%s asset=%s): %v", repo.TenantID(), assetID, err)
		return nil, "", http.StatusBadGateway, gin.H{"detail": "The compétence could not be extracted from the model act"}
	}
	if len(answer.Sections) == 0 {
		return nil, "", http.StatusBadGateway, gin.H{"detail": "The model act yielded no usable structure"}
	}

	encoded, err := json.Marshal(answer)
	if err != nil {
		return nil, "", http.StatusBadGateway, gin.H{"detail": "The extracted compétence could not be encoded"}
	}
	skillsLog.Infof("Compétence extracted (tenant=%s asset=%s sections=%d clauses=%d checks=%d)",
		repo.TenantID(), assetID, len(answer.Sections), len(answer.MandatoryClauses), len(answer.Checklist))
	return types.JSONB(encoded), cfg.ExtractionModel, 0, nil
}

// bytesAreJSONNull reports whether a submitted jsonb body is the literal
// `null` — which binds as a non-empty JSONB but carries no definition.
func bytesAreJSONNull(b types.JSONB) bool {
	return strings.TrimSpace(string(b)) == "null"
}
