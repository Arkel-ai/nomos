import { fetchApi } from './fetchApi';
import { queryString, unwrapRun as unwrap } from './runs';
import type { AgentCitationList, AgentCitationDetail, CoverageReport, SourceLocator } from './agent-contracts';

export async function listRunCitations(runId: string, query: {
	document_revision_id?: string;
	message_id?: string;
	after?: number;
	limit?: number;
} = {}): Promise<AgentCitationList> {
	return unwrap(await fetchApi<AgentCitationList>(`/runs/${runId}/citations${queryString({ ...query })}`));
}

export async function getCitation(id: string): Promise<AgentCitationDetail> {
	return unwrap(await fetchApi<AgentCitationDetail>(`/citations/${id}`));
}

export async function getRunCoverage(runId: string): Promise<CoverageReport> {
	return unwrap(await fetchApi<CoverageReport>(`/runs/${runId}/coverage`));
}

type Translate = (key: string, payload?: Record<string, unknown>) => string;

/** A cell in spreadsheet notation: `Récap!B4`, `'Salaire de référence'!B8`. */
export function excelReference(sheet: string | undefined, cell: string | undefined): string {
	if (!cell) return sheet ?? '';
	if (!sheet) return cell;
	const bare = /^[\p{L}_][\p{L}\p{N}_.]*$/u.test(sheet);
	return `${bare ? sheet : `'${sheet.split("'").join("''")}'`}!${cell}`;
}

/**
 * Where a citation points, for display. A DOCX paragraph reads `§ 4` and an
 * XLSX cell `Feuille!B4` — never a page number, which neither format has.
 * A locator naming no place reads as nothing.
 */
export function formatLocator(value: SourceLocator | string | undefined, tx: Translate, number: (n: number) => string = String): string {
	if (!value) return '';
	if (typeof value === 'string') return value;
	switch (value.kind) {
		case 'pdf': return tx('citations.page', { number: number(value.page) }) + (value.paragraph ? ` · ${tx('citations.paragraphMark', { number: number(value.paragraph) })}` : '');
		case 'docx': return value.paragraph ? tx('citations.paragraphMark', { number: number(value.paragraph) }) : '';
		case 'xlsx': return value.cell ? excelReference(value.sheet, value.cell) : value.sheet ? tx('citations.sheet', { name: value.sheet }) : '';
		case 'legislation': return tx('citations.versionId', { version: value.version_id }) + (value.paragraph === undefined ? '' : ` · ${tx('citations.paragraph', { number: number(value.paragraph) })}`);
		case 'decision': return value.paragraph === undefined ? '' : tx('citations.paragraph', { number: number(value.paragraph) });
		case 'web': return value.url;
		default: return '';
	}
}
