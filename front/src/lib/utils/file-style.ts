import type { SolarIconName } from '$lib/icons/solar';

export type FileStyle = { icon: SolarIconName; text: string; bg: string };

/**
 * Maps a file name (or folder) to an icon + theme-aware color classes.
 * Shared by the composer and the pièces table so the two stay in sync.
 * Colors are token-only: semantic file types get accent tokens
 * (destructive / pos / neg / violet / amber), generic ones get chart tints.
 */
export function fileStyle(name: string, isFolder = false): FileStyle {
	if (isFolder) {
		return { icon: 'folder', text: 'text-chart-2', bg: 'bg-chart-2/10' };
	}
	const ext = name.split('.').pop()?.toLowerCase() ?? '';
	switch (ext) {
		case 'pdf':
			return { icon: 'file-text', text: 'text-destructive', bg: 'bg-destructive/10' };
		case 'doc':
		case 'docx':
			return { icon: 'file-text', text: 'text-chart-2', bg: 'bg-chart-2/10' };
		case 'md':
		case 'markdown':
			return { icon: 'file-text', text: 'text-muted-foreground', bg: 'bg-muted-foreground/10' };
		case 'xls':
		case 'xlsx':
		case 'csv':
			return { icon: 'bill-list', text: 'text-pos', bg: 'bg-pos/10' };
		case 'ppt':
		case 'pptx':
			return { icon: 'presentation-graph', text: 'text-neg', bg: 'bg-neg/10' };
		case 'png':
		case 'jpg':
		case 'jpeg':
		case 'gif':
		case 'webp':
		case 'svg':
			return { icon: 'gallery', text: 'text-violet', bg: 'bg-violet/10' };
		case 'mp3':
		case 'wav':
		case 'flac':
		case 'm4a':
			return { icon: 'music-note', text: 'text-chart-4', bg: 'bg-chart-4/10' };
		case 'mp4':
		case 'mov':
		case 'webm':
		case 'avi':
			return { icon: 'videocamera', text: 'text-chart-3', bg: 'bg-chart-3/10' };
		case 'zip':
		case 'tar':
		case 'gz':
		case 'rar':
		case '7z':
			return { icon: 'archive', text: 'text-amber', bg: 'bg-amber/10' };
		case 'ts':
		case 'tsx':
		case 'js':
		case 'jsx':
		case 'json':
		case 'html':
		case 'css':
		case 'svelte':
		case 'vue':
		case 'py':
		case 'go':
		case 'rs':
			return { icon: 'file-code', text: 'text-chart-5', bg: 'bg-chart-5/10' };
		default:
			return { icon: 'file', text: 'text-muted-foreground', bg: 'bg-muted-foreground/10' };
	}
}
