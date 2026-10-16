/**
 * The composer's public contract (docs/PLAN.md §11). One component serves the
 * home (`/chat`) and the thread (`/chat/[id]`), so these shapes are the
 * boundary between the two pages and the input surface.
 */

/** One document staged in the composer. */
export interface Attachment {
	/** Server id — what `MessageCreateRequest.attachment_ids` carries. A local id while staged. */
	id: string;
	name: string;
	mime: string;
	/** Bytes. */
	size: number;
	/** Set while staged on this device: the host files it into the dossier on send. */
	file?: File;
}

/** A compétence as the selector and the `/` menu need it. */
export interface ComposerSkill {
	slug: string;
	name: string;
	description?: string;
}

/** What `onsubmit` receives. */
export interface ComposerPayload {
	text: string;
	attachments: Attachment[];
	skill: string | null;
}
