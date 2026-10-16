<script lang="ts">
	import { untrack } from 'svelte';
	import { prefersReducedMotion } from 'svelte/motion';

	/**
	 * A reply that grows token by token.
	 *
	 * Deltas arrive in bursts. Rather than paint each burst as it lands, a frame
	 * loop releases whole words at a rate that rises with the backlog, so the
	 * text flows evenly and never trails the stream by more than a beat. Only
	 * the newest words animate in (blur → sharp, `.word-in`); a word becomes
	 * plain settled text once its animation is over, so a long reply costs no
	 * more DOM than a short one.
	 *
	 * Once the stream ends and the reveal has caught up, the reply is shown
	 * with light structure — paragraphs, bullet and numbered lists, **bold** —
	 * built from text nodes, never injected HTML.
	 */

	let {
		text,
		streaming = false,
		class: className = ''
	}: { text: string; streaming?: boolean; class?: string } = $props();

	type Fresh = { id: number; text: string; at: number };

	// A thread reloaded mid-stream starts from what is already persisted.
	let revealed = $state(untrack(() => (streaming ? 0 : text.length)));
	let settled = $state(untrack(() => (streaming ? '' : text)));
	let fresh = $state<Fresh[]>([]);
	let nextId = 0;

	const WORD_MS = 420;
	const live = $derived(streaming || revealed < text.length || fresh.length > 0);

	// One loop for the life of the stream. It reads the current `text` each
	// frame, so new deltas never restart it (a restart would reset the pace).
	let frame = 0;
	let last = 0;
	let budget = 0;

	function tick(now: number) {
		const dt = Math.min(0.1, (now - last) / 1000);
		last = now;

		const pending = text.slice(revealed).match(/\s*\S+/g)?.length ?? 0;
		// ~28 words/s at rest, faster as the backlog grows.
		budget = Math.min(3, budget + dt * (28 + pending * 9));

		while (budget >= 1 && revealed < text.length) {
			const next = text.slice(revealed).match(/^\s*\S+/);
			// Hold a trailing partial word until the stream finishes it.
			if (!next || (streaming && revealed + next[0].length === text.length)) break;
			fresh.push({ id: nextId++, text: next[0], at: now });
			revealed += next[0].length;
			budget -= 1;
		}

		// Words whose entry animation is over become plain text.
		let drop = 0;
		while (drop < fresh.length && now - fresh[drop].at > WORD_MS) drop++;
		if (drop) {
			settled += fresh
				.slice(0, drop)
				.map((w) => w.text)
				.join('');
			fresh = fresh.slice(drop);
		}

		if (streaming || revealed < text.length || fresh.length) {
			frame = requestAnimationFrame(tick);
		} else {
			frame = 0;
			settled = text; // trailing whitespace the word matcher never consumed
		}
	}

	$effect(() => {
		// Re-arm whenever new text lands or the stream state changes.
		void text;
		void streaming;
		const reduced = prefersReducedMotion.current;
		untrack(() => {
			// The persisted text wins over the deltas: if it is not a
			// continuation of what is on screen, resync from it.
			const shown = settled + fresh.map((w) => w.text).join('');
			if (!text.startsWith(shown)) {
				revealed = Math.min(revealed, text.length);
				settled = text.slice(0, revealed);
				fresh = [];
			}
			if (reduced) {
				cancelAnimationFrame(frame);
				frame = 0;
				revealed = text.length;
				settled = text;
				fresh = [];
				return;
			}
			if (!frame && (streaming || revealed < text.length || fresh.length)) {
				last = performance.now();
				frame = requestAnimationFrame(tick);
			}
		});
	});

	$effect(() => () => cancelAnimationFrame(frame));

	// ── Light structure for the settled reply ────────────────────────────────
	type Inline = { bold: boolean; text: string };
	type Block =
		| { kind: 'p'; lines: Inline[][] }
		| { kind: 'ul' | 'ol'; items: Inline[][] };

	function inline(line: string): Inline[] {
		return line
			.split(/(\*\*[^*]+\*\*)/g)
			.filter(Boolean)
			.map((part) =>
				part.startsWith('**') && part.endsWith('**') && part.length > 4
					? { bold: true, text: part.slice(2, -2) }
					: { bold: false, text: part }
			);
	}

	function blocks(source: string): Block[] {
		const out: Block[] = [];
		for (const chunk of source.trim().split(/\n\s*\n/)) {
			const lines = chunk.split('\n').filter((l) => l.trim());
			if (!lines.length) continue;
			if (lines.every((l) => /^\s*[-•*]\s+/.test(l))) {
				out.push({ kind: 'ul', items: lines.map((l) => inline(l.replace(/^\s*[-•*]\s+/, ''))) });
			} else if (lines.every((l) => /^\s*\d+[.)]\s+/.test(l))) {
				out.push({ kind: 'ol', items: lines.map((l) => inline(l.replace(/^\s*\d+[.)]\s+/, ''))) });
			} else {
				out.push({ kind: 'p', lines: lines.map(inline) });
			}
		}
		return out;
	}

	const structured = $derived(live ? [] : blocks(text));
</script>

{#snippet line(parts: Inline[])}
	{#each parts as part}{#if part.bold}<strong class="font-semibold">{part.text}</strong
			>{:else}{part.text}{/if}{/each}
{/snippet}

{#if live}
	<p class="whitespace-pre-wrap {className}">
		{settled}{#each fresh as word (word.id)}<span class="word-in">{word.text}</span
			>{/each}<span class="signal-caret" aria-hidden="true"></span>
	</p>
{:else}
	<div class="space-y-3 {className}">
		{#each structured as block}
			{#if block.kind === 'p'}
				<p>
					{#each block.lines as parts, i}{#if i > 0}<br />{/if}{@render line(parts)}{/each}
				</p>
			{:else if block.kind === 'ul'}
				<ul class="list-disc space-y-1 pl-5 marker:text-muted-foreground">
					{#each block.items as parts}<li>{@render line(parts)}</li>{/each}
				</ul>
			{:else}
				<ol class="list-decimal space-y-1 pl-5 marker:text-muted-foreground">
					{#each block.items as parts}<li>{@render line(parts)}</li>{/each}
				</ol>
			{/if}
		{/each}
	</div>
{/if}
