import { uploadPieces } from '$lib/api/pieces';
import type { Attachment } from './types';

/**
 * Files dropped on the composer are staged here, on the device, and only
 * become pièces when the turn is sent — the host then knows which dossier
 * they go to (the bound one, or one it creates for them).
 */
export function stageFiles(files: File[]): Attachment[] {
	return files.map((file) => ({
		id: crypto.randomUUID(),
		name: file.name,
		mime: file.type,
		size: file.size,
		file
	}));
}

/**
 * Uploads the staged attachments into the dossier and returns the list with
 * each one replaced by its pièce, in place. Refused files leave the list and
 * are counted, so the host can say so. Already-filed attachments pass through.
 */
export async function fileStaged(
	dossierId: string,
	attachments: Attachment[]
): Promise<{ attachments: Attachment[]; rejected: number }> {
	const staged = attachments.filter((a) => a.file);
	if (!staged.length) return { attachments, rejected: 0 };

	const result = await uploadPieces(
		dossierId,
		staged.map((a) => a.file as File)
	);
	// The pièce row carries the name the file arrived with: match on it.
	const pieces = new Map(result.accepted.map((p) => [p.original_name, p]));
	const filed: Attachment[] = [];
	for (const a of attachments) {
		if (!a.file) {
			filed.push(a);
			continue;
		}
		const piece = pieces.get(a.name);
		if (!piece) continue;
		filed.push({
			id: piece.id,
			name: piece.final_name || piece.proposed_name || piece.original_name,
			mime: a.mime,
			size: a.size
		});
	}
	return { attachments: filed, rejected: result.rejected.length };
}
