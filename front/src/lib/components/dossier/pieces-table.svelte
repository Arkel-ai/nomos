<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// The pièces table — grouped by rubrique, ordered by ordinal.
	//
	// PLAN §1 / §7.1 make this a hard requirement: EVERY proposed field is
	// editable by the lawyer before export. So there is no read mode — the
	// ordinal, the type de pièce, the date de l'acte and the titre are live
	// inputs that persist through `correctPiece` (optimistic, rolls back).
	import { t } from '$lib/i18n';
	import * as Table from '$lib/components/ui/table';
	import * as Select from '$lib/components/ui/select';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import EmptyState from '$lib/components/empty-state.svelte';
	import DateField from '$lib/components/date-field.svelte';
	import { fileStyle } from '$lib/utils/file-style';
	import ConfidenceBadge from './confidence-badge.svelte';
	import { correctPiece } from '$lib/stores/dossier';
	import { downloadPiece, pieceKinds, type Piece, type PieceState } from '$lib/api/pieces';
	import { sileo } from '$lib/components/ui/sileo';

	let { pieces, loading = false }: { pieces: Piece[]; loading?: boolean } = $props();

	// Soft chips with a dot: a proposal waits on the lawyer (amber dot), an
	// approval is calm green, analysis is the machine at work (Outremer).
	const STATE_CHIP: Record<PieceState, string> = {
		pending: 'bg-brand-soft text-brand-soft-foreground',
		extracted: 'bg-brand-soft text-brand-soft-foreground',
		proposed: 'bg-muted text-foreground [--dot:var(--amber)]',
		approved: 'bg-pos-soft text-pos-soft-foreground [--dot:var(--pos)]',
		rejected: 'bg-muted text-muted-foreground line-through [--dot:var(--muted-foreground)]',
		failed: 'bg-destructive/10 text-destructive [--dot:var(--destructive)]'
	};

	// Inline-editable fields stay quiet until engaged: no box at rest, a
	// hairline on hover, the focus ring when editing.
	const QUIET =
		'border-transparent bg-transparent shadow-none hover:border-border focus-visible:border-ring dark:bg-transparent'

	/** Stored, but the runner has not finished with it: extraction, chunking
	 *  and qualification are still to come, so the type, the date and the titre
	 *  are legitimately empty rather than missing. */
	const analysing = (p: Piece) =>
		p.classification_state === 'pending' || p.classification_state === 'extracted';

	/** The classement step writes a rubrique into the extraction payload; until
	 *  it has run, the type de pièce is the honest grouping. */
	function rubriqueOf(p: Piece): string {
		const r = (p.extraction as { rubrique?: unknown } | undefined)?.rubrique;
		if (typeof r === 'string' && r.trim()) return r.trim();
		return p.piece_kind ? $t(`pieces.kinds.${p.piece_kind}`) : $t('pieces.rubriqueDefault');
	}

	const groups = $derived.by(() => {
		const byRubrique = new Map<string, Piece[]>();
		for (const p of [...pieces].sort((a, b) => a.ordinal - b.ordinal)) {
			const key = rubriqueOf(p);
			byRubrique.set(key, [...(byRubrique.get(key) ?? []), p]);
		}
		return [...byRubrique.entries()].map(([rubrique, items]) => ({ rubrique, items }));
	});

	// ---- inline editing -------------------------------------------------
	let saving = $state<string | null>(null);

	async function save(piece: Piece, patch: Parameters<typeof correctPiece>[1]) {
		saving = piece.id;
		try {
			await correctPiece(piece.id, patch);
		} catch {
			sileo.error({ title: $t('pieces.saveError') });
		} finally {
			saving = null;
		}
	}

	async function download(piece: Piece) {
		try {
			await downloadPiece(piece.id);
		} catch {
			sileo.error({ title: $t('artifacts.downloadError') });
		}
	}
</script>

{#snippet head(label: string, cls = '')}
	<Table.Head
		class="text-muted-foreground h-9 text-[10px] font-medium tracking-[0.1em] whitespace-nowrap uppercase {cls}"
	>
		{label}
	</Table.Head>
{/snippet}

{#if loading}
	<div class="space-y-2 py-2">
		{#each { length: 6 } as _}
			<Skeleton class="h-14 w-full rounded-xl" />
		{/each}
	</div>
{:else if !pieces.length}
	<EmptyState icon="documents" title={$t('pieces.none')} hint={$t('pieces.uploadHint')} />
{:else}
	<div class="space-y-2">
		<div class="text-muted-foreground text-xs">{$t('pieces.inlineHint')}</div>
		<div class="-mx-2 overflow-x-auto">
			<Table.Root class="[&_thead]:bg-transparent">
				<Table.Header class="bg-transparent">
					<Table.Row class="border-border hover:bg-transparent">
						{@render head($t('pieces.columns.ordinal'), 'w-[1%] pl-2')}
						{@render head($t('pieces.columns.name'))}
						{@render head($t('pieces.columns.kind'))}
						{@render head($t('pieces.columns.date'))}
						{@render head($t('pieces.columns.confidence'))}
						{@render head($t('pieces.columns.state'))}
						<Table.Head class="w-[1%]"></Table.Head>
					</Table.Row>
				</Table.Header>
				<Table.Body>
					{#each groups as group (group.rubrique)}
						<Table.Row class="border-0 hover:bg-transparent">
							<Table.Cell colspan={7} class="px-2 pt-6 pb-2">
								<div class="flex items-center gap-2.5">
									<span class="text-foreground text-2xs font-semibold tracking-[0.08em] uppercase">
										{group.rubrique}
									</span>
									<span class="text-muted-foreground font-mono text-[10px] tabular-nums">
										{group.items.length}
									</span>
									<span class="bg-border h-px flex-1" aria-hidden="true"></span>
								</div>
							</Table.Cell>
						</Table.Row>
						{#each group.items as piece (piece.id)}
							{@const file = fileStyle(piece.original_name)}
							<!-- Every control sits on one 32px line at the top of the row;
							     only the title cell carries a second line (the file name). -->
							<Table.Row
								class="group/row border-border/50 hover:bg-muted/40 transition-colors [&>td]:py-2.5 [&>td]:align-top {piece.classification_state ===
								'rejected'
									? 'opacity-55'
									: ''}"
							>
								<Table.Cell class="w-[1%] pl-2">
									<Input
										type="number"
										min="1"
										value={piece.ordinal}
										disabled={saving === piece.id}
										aria-label={$t('pieces.columns.ordinal')}
										class="bg-muted/70 hover:border-border focus-visible:border-ring dark:bg-muted/70 h-8 w-12 [appearance:textfield] border-transparent px-1 text-center font-mono text-ui font-medium tabular-nums shadow-none [&::-webkit-inner-spin-button]:appearance-none"
										onchange={(e) =>
											save(piece, { ordinal: Number((e.currentTarget as HTMLInputElement).value) })}
									/>
								</Table.Cell>
								<Table.Cell class="min-w-[280px]">
									<div class="flex items-start gap-3">
										<span
											class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg {file.bg} {file.text}"
										>
											<Icon name={file.icon} class="h-4 w-4" />
										</span>
										<div class="min-w-0 flex-1">
											<Input
												value={piece.title}
												disabled={saving === piece.id}
												aria-label={$t('pieces.columns.name')}
												class="-ml-2 h-8 text-ui font-medium {QUIET}"
												onchange={(e) =>
													save(piece, { title: (e.currentTarget as HTMLInputElement).value })}
											/>
											<div class="text-muted-foreground truncate font-mono text-[10.5px] leading-4">
												{piece.original_name}
											</div>
										</div>
									</div>
								</Table.Cell>
								<Table.Cell class="min-w-[170px]">
									<Select.Root
										type="single"
										value={piece.piece_kind}
										onValueChange={(v) => save(piece, { piece_kind: v })}
									>
										<Select.Trigger class="-ml-2 h-8 w-full text-ui {QUIET}">
											{piece.piece_kind
												? $t(`pieces.kinds.${piece.piece_kind}`)
												: $t('common.none')}
										</Select.Trigger>
										<Select.Content>
											{#each pieceKinds as kind}
												<Select.Item value={kind}>{$t(`pieces.kinds.${kind}`)}</Select.Item>
											{/each}
										</Select.Content>
									</Select.Root>
								</Table.Cell>
								<Table.Cell class="w-[1%] min-w-[150px]">
									<DateField
										value={piece.document_date}
										disabled={saving === piece.id}
										label={$t('pieces.columns.date')}
										class="-ml-2"
										onchange={(seconds) => save(piece, { document_date: seconds })}
									/>
								</Table.Cell>
								<Table.Cell>
									<div class="flex h-8 items-center">
										<ConfidenceBadge confidence={piece.confidence} />
									</div>
								</Table.Cell>
								<Table.Cell>
									<div class="flex h-8 items-center">
										<span
											class="inline-flex h-5 items-center gap-1.5 rounded-full px-2 text-2xs font-medium whitespace-nowrap {STATE_CHIP[
												piece.classification_state
											] ?? STATE_CHIP.proposed}"
										>
											{#if analysing(piece)}
												<Spinner class="h-3 w-3" />
											{:else}
												<span class="h-1.5 w-1.5 rounded-full bg-[var(--dot)]" aria-hidden="true"></span>
											{/if}
											{$t(`pieces.state.${piece.classification_state}`)}
										</span>
									</div>
								</Table.Cell>
								<Table.Cell class="w-[1%] pr-2 whitespace-nowrap">
									<div
										class="flex h-8 items-center justify-end gap-0.5 opacity-0 transition-opacity group-hover/row:opacity-100 focus-within:opacity-100"
									>
										<Tooltip.Root>
											<Tooltip.Trigger>
												{#snippet child({ props })}
													<Button
														{...props}
														variant="ghost"
														size="icon"
														class="text-muted-foreground hover:text-foreground h-8 w-8"
														aria-label={$t('pieces.approve')}
														disabled={piece.classification_state === 'approved'}
														onclick={() => save(piece, { classification_state: 'approved' })}
													>
														<Icon
															name="check-circle"
															active={piece.classification_state === 'approved'}
															class="h-4 w-4 {piece.classification_state === 'approved' ? 'text-pos' : ''}"
														/>
													</Button>
												{/snippet}
											</Tooltip.Trigger>
											<Tooltip.Content>{$t('pieces.approve')}</Tooltip.Content>
										</Tooltip.Root>
										<Tooltip.Root>
											<Tooltip.Trigger>
												{#snippet child({ props })}
													<Button
														{...props}
														variant="ghost"
														size="icon"
														class="text-muted-foreground hover:text-foreground h-8 w-8"
														aria-label={$t('pieces.reject')}
														disabled={piece.classification_state === 'rejected'}
														onclick={() => save(piece, { classification_state: 'rejected' })}
													>
														<Icon name="forbidden-circle" class="h-4 w-4" />
													</Button>
												{/snippet}
											</Tooltip.Trigger>
											<Tooltip.Content>{$t('pieces.reject')}</Tooltip.Content>
										</Tooltip.Root>
										<Tooltip.Root>
											<Tooltip.Trigger>
												{#snippet child({ props })}
													<Button
														{...props}
														variant="ghost"
														size="icon"
														class="text-muted-foreground hover:text-foreground h-8 w-8"
														aria-label={$t('pieces.download')}
														onclick={() => download(piece)}
													>
														<Icon name="download" class="h-4 w-4" />
													</Button>
												{/snippet}
											</Tooltip.Trigger>
											<Tooltip.Content>{$t('pieces.download')}</Tooltip.Content>
										</Tooltip.Root>
									</div>
								</Table.Cell>
							</Table.Row>
						{/each}
					{/each}
				</Table.Body>
			</Table.Root>
		</div>
	</div>
{/if}
