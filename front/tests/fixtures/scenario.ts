/**
 * The fictional Vasseur matter, as the agent contracts describe it. Every name,
 * date and source below is invented for the suite — nothing here is a real
 * party, a real decision or a real article. Shapes are typed from
 * src/lib/api/agent-contracts.ts, so a contract change breaks the suite at
 * `npm run check` rather than silently at runtime.
 */
import type {
	AgentActivityPayload,
	AgentCitation,
	AgentCitationDetail,
	CoverageReport,
	PieceCitation
} from '../../src/lib/api/agent-contracts';
import type { Artifact, RunQuestion } from '../../src/lib/api/runs';
import type { Skill } from '../../src/lib/api/skills';
import type { DossierDetail } from '../../src/lib/api/dossiers';
import type { Piece } from '../../src/lib/api/pieces';

export const TENANT = 'tenant-e2e';
export const USER_ID = 'user-e2e';
export const DOSSIER_ID = 'dos-vasseur';

export const now = () => Math.floor(Date.now() / 1000);

/** The 18 pièces of the fictional matter (same names as the chat-thread mockup). */
export const VASSEUR_FILES = [
	'01-contrat-de-travail-cdi-2019.pdf',
	'02-avenant-n1-chef-equipe-quai-2022.pdf',
	'03-avertissement-2023-11-14.pdf',
	'04-courriel-alerte-sous-effectif-2025-03-06.pdf',
	'05-journal-de-quai-17-mars-2025.pdf',
	'06-transcription-sms-2025-03-17.docx',
	'07-notification-mise-a-pied-conservatoire-2025-03-20.pdf',
	'08-convocation-entretien-prealable-2025-03-24.pdf',
	'09-avis-de-reception-convocation.pdf',
	'10-compte-rendu-entretien-prealable-2025-03-31.docx',
	'11-lettre-de-licenciement-faute-grave-2025-04-07.pdf',
	'12-bulletin-de-paie-mars-2025.pdf',
	'13-solde-de-tout-compte-2025-04-07.pdf',
	'14-attestation-france-travail.pdf',
	'15-attestation-temoin-lemoine.docx',
	'16-attestation-temoin-arbogast.docx',
	'17-recapitulatif-bulletins-de-paie.xlsx',
	'18-note-de-chiffrage-demandes.xlsx'
];

export function vasseurDossier(): DossierDetail {
	const at = now() - 86_400;
	return {
		id: DOSSIER_ID,
		tenant_id: TENANT,
		title: 'Vasseur c/ Logistique Exemple (fictif)',
		client_reference: 'E2E-001',
		matter_type: 'travail',
		state: 'active',
		title_pending: false,
		created_by: USER_ID,
		purge_after: at + 10 * 365 * 86_400,
		created_at: at,
		updated_at: at,
		piece_count: VASSEUR_FILES.length,
		approved_count: VASSEUR_FILES.length,
		run_count: 1
	};
}

export function vasseurPieces(): Piece[] {
	const at = now() - 86_400;
	return VASSEUR_FILES.map((name, i) => ({
		id: `piece-${i + 1}`,
		tenant_id: TENANT,
		dossier_id: DOSSIER_ID,
		source_asset_id: `asset-${i + 1}`,
		original_name: name,
		proposed_name: name,
		final_name: name,
		ordinal: i + 1,
		document_date: null,
		date_precision: 'unknown',
		piece_kind: 'autre',
		title: name,
		classification_state: 'approved',
		confidence: 1,
		extraction_version: 'e2e',
		page_count: 4,
		text_chars: 4000,
		created_at: at,
		updated_at: at
	}));
}

export const SKILLS: Skill[] = [
	{
		id: 'skill-cph',
		tenant_id: TENANT,
		slug: 'requete-cph',
		name: 'Requête CPH (projet)',
		description: 'Requête introductive devant le conseil de prud’hommes',
		state: 'active',
		current_version: 1,
		created_by: USER_ID,
		created_at: 0,
		updated_at: 0
	},
	{
		id: 'skill-med',
		tenant_id: TENANT,
		slug: 'mise-en-demeure',
		name: 'Mise en demeure',
		description: 'Lettre de mise en demeure',
		state: 'active',
		current_version: 1,
		created_by: USER_ID,
		created_at: 0,
		updated_at: 0
	}
];

// ---------------------------------------------------------------------------
// The answer
// ---------------------------------------------------------------------------

export const ANSWER = [
	'Chronologie établie à partir des 18 pièces du dossier Vasseur (fictif).',
	'',
	'- 2 septembre 2019 : embauche en contrat à durée indéterminée.',
	'- 17 mars 2025 : échange de SMS sur le départ anticipé du quai.',
	'- 24 mars 2025 : convocation à l’entretien préalable.',
	'- 7 avril 2025 : notification du licenciement pour faute grave.',
	'',
	'Un point reste à vérifier dans la pièce n° 6 : la date du message n’a pas été retrouvée dans son texte.'
].join('\n');

/** The answer in the bursts the server would append, cut mid-word on purpose. */
export function answerDeltas(text = ANSWER, parts = 6): string[] {
	const size = Math.ceil(text.length / parts);
	const out: string[] = [];
	for (let i = 0; i < text.length; i += size) out.push(text.slice(i, i + size));
	return out;
}

// ---------------------------------------------------------------------------
// Activity (owner decision 1): one row per call, grouped by dispatch
// ---------------------------------------------------------------------------

type Call = Omit<AgentActivityPayload, 'status' | 'at'> & {
	final: AgentActivityPayload['status'];
};

/** Eight dispatches of a chronology request, including a failed then retried search. */
export const CHRONOLOGY_CALLS: Call[] = [
	{
		dispatch: 1,
		call_id: 'call-1-1',
		tool: 'list_pieces',
		progress_note: 'Je recense les pièces du dossier avant de les lire.',
		object: { kind: 'pieces', label: 'Dossier Vasseur · 18 pièces' },
		counts: { total: 18 },
		final: 'ok'
	},
	{
		dispatch: 2,
		call_id: 'call-2-1',
		tool: 'read_piece',
		progress_note: 'Je lis le contrat de travail et la transcription des SMS.',
		object: { kind: 'piece', label: 'Pièce n° 1 · Contrat de travail', detail: 'Pages 1–4' },
		counts: { segments: 4 },
		final: 'ok'
	},
	{
		dispatch: 2,
		call_id: 'call-2-2',
		tool: 'read_piece',
		progress_note: 'Je lis le contrat de travail et la transcription des SMS.',
		object: { kind: 'piece', label: 'Pièce n° 6 · Transcription des SMS', detail: 'Paragraphes 1–4' },
		counts: { segments: 4 },
		final: 'ok'
	},
	{
		dispatch: 3,
		call_id: 'call-3-1',
		tool: 'search_law',
		progress_note: 'Je recherche les textes applicables à la convocation.',
		object: { kind: 'query', label: 'Légifrance · délai de convocation à l’entretien préalable' },
		final: 'error'
	},
	{
		dispatch: 4,
		call_id: 'call-4-1',
		tool: 'search_law',
		progress_note: 'Je reprends la recherche interrompue.',
		object: { kind: 'query', label: 'Légifrance · délai de convocation à l’entretien préalable' },
		counts: { results: 2, total: 2 },
		final: 'ok'
	},
	{
		dispatch: 5,
		call_id: 'call-5-1',
		tool: 'search_caselaw',
		progress_note: 'Je cherche une décision sur la faute grave et le départ anticipé.',
		object: { kind: 'query', label: 'Judilibre · faute grave et départ anticipé' },
		counts: { results: 0 },
		final: 'empty'
	},
	{
		dispatch: 6,
		call_id: 'call-6-1',
		tool: 'search_web',
		progress_note: 'Je consulte une documentation publique complémentaire.',
		object: { kind: 'query', label: 'Web · délai de convocation, documentation publique' },
		counts: { results: 1 },
		final: 'ok'
	},
	{
		dispatch: 7,
		call_id: 'call-7-1',
		tool: 'verify_output',
		progress_note: 'Je contrôle les références de la chronologie.',
		object: { kind: 'verification', label: 'Chronologie · révision 1' },
		counts: { warnings: 2, blocking: 0 },
		final: 'ok'
	},
	{
		dispatch: 8,
		call_id: 'call-8-1',
		tool: 'submit_result',
		progress_note: 'Je remets la chronologie vérifiée.',
		object: { kind: 'submission', label: 'Chronologie du dossier' },
		final: 'ok'
	}
];

/** The `running` row, then its settled row under the same call_id. */
export function activityRows(call: Call): [AgentActivityPayload, AgentActivityPayload] {
	const { final, counts, ...rest } = call;
	return [
		{ ...rest, status: 'running', at: now() },
		{ ...rest, status: final, ...(counts ? { counts } : {}), at: now() }
	];
}

// ---------------------------------------------------------------------------
// Citations: one per source type, all fictional
// ---------------------------------------------------------------------------

export const CITATION_IDS = {
	piece: 'cit-piece-6',
	legifrance: 'cit-legi-demo',
	judilibre: 'cit-juri-demo',
	web: 'cit-web-demo'
} as const;

/** An SMS transcript passage: the emoji before the quote checks code-point offsets. */
const PIECE_QUOTE = 'Je quitte le quai maintenant, le chef d’équipe est prévenu.';
const PIECE_BEFORE = '17/03 08:12 — 👍 Bien reçu. 17/03 08:14 — ';
const PIECE_AFTER = ' 17/03 08:15 — D’accord.';
const LEGI_QUOTE = 'Texte fictif utilisé uniquement pour montrer la présentation d’une version.';
const JURI_QUOTE = 'Passage fictif de la décision de démonstration.';
const WEB_QUOTE = 'Extrait fictif consulté sur une page publique.';

export function chronologyCitations(messageId: string): AgentCitation[] {
	const at = now() - 120;
	const common = {
		claim_ids: ['claim-1'],
		message_id: messageId,
		document_revision_id: null,
		evidence_span_id: 'span-1',
		source_sha256: 'e2e',
		retrieved_at: at,
		checked_at: at
	};
	const piece: PieceCitation = {
		...common,
		id: CITATION_IDS.piece,
		number: 1,
		source_type: 'piece',
		piece_id: 'piece-6',
		piece_ordinal: 6,
		filename: VASSEUR_FILES[5],
		label: 'Pièce n° 6 · Transcription des SMS',
		quote: PIECE_QUOTE,
		locator: { kind: 'docx', paragraph: 2, start: 0, end: [...PIECE_QUOTE].length },
		checks: [{ check: 'quote_matched', status: 'passed' }],
		// The date attributed to the pièce was not found: a warning, never a block.
		warning_codes: ['piece_date_not_found']
	};
	return [
		piece,
		{
			...common,
			id: CITATION_IDS.legifrance,
			number: 2,
			source_type: 'legifrance',
			label: 'Article de démonstration — sans valeur juridique',
			quote: LEGI_QUOTE,
			locator: { kind: 'legislation', version_id: 'DEMO-V1', start: 0, end: [...LEGI_QUOTE].length },
			article_label: 'Article DEMO-1',
			valid_from: 1672531200,
			valid_to: 1735689600,
			url: 'https://example.invalid/legifrance/demo',
			checks: [
				{ check: 'source_resolved', status: 'passed' },
				{ check: 'temporal_valid', status: 'passed' },
				{ check: 'official_matched', status: 'unavailable' }
			],
			warning_codes: []
		},
		{
			...common,
			id: CITATION_IDS.judilibre,
			number: 3,
			source_type: 'judilibre',
			label: 'Décision de démonstration — entièrement fictive',
			quote: JURI_QUOTE,
			locator: {
				kind: 'decision',
				decision_id: 'DEMO-DEC',
				paragraph: 12,
				start: 0,
				end: [...JURI_QUOTE].length
			},
			court: 'Juridiction fictive',
			chamber: 'Chambre de démonstration',
			decision_date: 1714521600,
			ecli: 'ECLI:FR:DEMO:2024:0001',
			pourvoi: 'DEMO-24-001',
			url: 'https://example.invalid/judilibre/demo',
			checks: [
				{ check: 'source_resolved', status: 'passed' },
				{ check: 'semantic_support', status: 'not_checked' }
			],
			warning_codes: []
		},
		{
			...common,
			id: CITATION_IDS.web,
			number: 4,
			source_type: 'web',
			label: 'Documentation publique — exemple fictif',
			quote: WEB_QUOTE,
			locator: {
				kind: 'web',
				url: 'https://example.invalid/information',
				start: 0,
				end: [...WEB_QUOTE].length
			},
			url: 'https://example.invalid/information',
			host: 'example.invalid',
			title: 'Documentation fictive',
			extent: 'excerpt',
			checks: [{ check: 'quote_matched', status: 'passed' }],
			warning_codes: []
		}
	];
}

/** GET /citations/:id — the frozen passage with the quote's code-point range. */
export function citationDetail(citation: AgentCitation, sourceAvailable = true): AgentCitationDetail {
	const before = citation.source_type === 'piece' ? PIECE_BEFORE : 'Contexte précédent du passage. ';
	const after = citation.source_type === 'piece' ? PIECE_AFTER : ' Suite du passage consulté.';
	const start = [...before].length;
	return {
		...citation,
		source_available: sourceAvailable,
		passage: sourceAvailable ? `${before}${citation.quote}${after}` : '',
		highlight_start: sourceAvailable ? start : 0,
		highlight_end: sourceAvailable ? start + [...citation.quote].length : 0
	};
}

/** Many sources, to exercise the 50-per-page list. */
export function manyCitations(messageId: string, count: number): AgentCitation[] {
	const [first] = chronologyCitations(messageId);
	return Array.from({ length: count }, (_, i) => ({
		...(first as PieceCitation),
		id: `cit-many-${i + 1}`,
		number: i + 1,
		label: `Pièce n° ${(i % 18) + 1} · extrait ${i + 1}`,
		quote: `Extrait fictif numéro ${i + 1}.`,
		warning_codes: []
	}));
}

// ---------------------------------------------------------------------------
// Coverage, artifacts, questions
// ---------------------------------------------------------------------------

export function fullCoverage(runId: string): CoverageReport {
	return {
		run_id: runId,
		scope_sha256: 'e2e',
		exhaustive: true,
		complete: true,
		total_pieces: 18,
		fully_read: 18,
		computed_at: now(),
		segments: { expected: 72, consumed: 72, missing: 0, pending: 0, unreadable: 0, excluded: 0 },
		pieces: VASSEUR_FILES.map((name, i) => ({
			piece_id: `piece-${i + 1}`,
			ordinal: i + 1,
			name,
			state: 'complete',
			unit: name.endsWith('.xlsx') ? 'cell' : name.endsWith('.docx') ? 'paragraph' : 'page',
			segments: { expected: 4, consumed: 4, missing: 0, pending: 0, unreadable: 0, excluded: 0 },
			mutated: false
		}))
	};
}

export function chronologyArtifact(runId: string): Artifact {
	return {
		id: 'art-chronologie',
		tenant_id: TENANT,
		run_id: runId,
		dossier_id: DOSSIER_ID,
		kind: 'note_docx',
		asset_id: 'asset-chronologie',
		name: 'Chronologie — dossier Vasseur (fictif).docx',
		char_count: 4210,
		template_version: '',
		renderer_version: 'e2e',
		approved: false,
		approved_by: '',
		approved_at: null,
		document_revision_id: 'rev-1',
		verification_id: 'ver-1',
		partial: false,
		piece_warnings: 2,
		created_at: now()
	};
}

/** ask_lawyer offering a compétence for a free-text acte (owner decision 5). */
export function competenceQuestion(runId: string): RunQuestion {
	return {
		id: 'q-competence',
		tenant_id: TENANT,
		run_id: runId,
		run_step_id: 'step-ask',
		question_key: 'competence',
		prompt: 'Cette requête relève d’une compétence. Laquelle dois-je appliquer ?',
		kind: 'single',
		options: [
			{ value: 'requete-cph', label: 'Requête CPH (projet)', hint: 'Modèle de requête prud’homale' },
			{ value: 'none', label: 'Aucune — une note seulement' }
		],
		required: true,
		state: 'pending',
		answered_by: '',
		asked_at: now(),
		answered_at: null
	};
}
