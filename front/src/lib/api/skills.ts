import { fetchApi } from './fetchApi';
import {
	queryString,
	unwrapRun as unwrap,
	type ListQuery,
	type ListResponse
} from './runs';

/**
 * Compétences — playbooks extracted from a model act (§8, §10).
 *
 * Skills are IMMUTABLE VERSIONS. `skills` holds identity and the current
 * version pointer; `skill_versions` holds the extracted definition, the source
 * asset, the reference .docx and the extractor model. A mutable definition
 * makes an earlier pleading unreproducible, and reproducing an earlier
 * pleading is the whole point — so the UI never offers "edit this version",
 * only "publish a new one".
 */

export type SkillState = 'draft' | 'active' | 'archived';

export interface Skill {
	id: string;
	tenant_id: string;
	slug: string;
	name: string;
	description: string;
	state: SkillState;
	current_version: number;
	created_by: string;
	created_at: number;
	updated_at: number;
}

export interface SkillVersion {
	id: string;
	tenant_id: string;
	skill_id: string;
	version: number;
	/** The model act the playbook was extracted from. */
	source_asset_id: string | null;
	/** The reference .docx carrying the firm's charte styles, for pandoc. */
	reference_asset_id: string | null;
	schema_version: number;
	definition?: Record<string, unknown>;
	extractor_model: string;
	created_by: string;
	created_at: number;
}

/** GET /skills/:id — identity plus its version history. */
export interface SkillDetail extends Skill {
	versions: SkillVersion[];
}

export interface SkillCreateRequest {
	slug: string;
	name: string;
	description?: string;
	source_asset_id?: string | null;
	reference_asset_id?: string | null;
	definition?: Record<string, unknown>;
}

/** GET /skills — the compétences library, paginated. */
export async function listSkills(query: ListQuery = {}): Promise<ListResponse<Skill>> {
	return unwrap(await fetchApi<ListResponse<Skill>>(`/skills${queryString({ ...query })}`));
}

/** POST /skills — creates the skill and its first immutable version. */
export async function createSkill(body: SkillCreateRequest): Promise<SkillDetail> {
	return unwrap(await fetchApi<SkillDetail>('/skills', 'POST', body));
}

/** GET /skills/:id */
export async function getSkill(id: string): Promise<SkillDetail> {
	return unwrap(await fetchApi<SkillDetail>(`/skills/${id}`));
}

/** DELETE /skills/:id — versions already used by a run are kept. */
export async function deleteSkill(id: string): Promise<void> {
	unwrap(await fetchApi(`/skills/${id}`, 'DELETE'));
}
