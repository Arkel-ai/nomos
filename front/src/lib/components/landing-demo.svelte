<script lang="ts">
	import { onMount } from 'svelte';
	import { prefersReducedMotion } from 'svelte/motion';
	import { t } from '$lib/i18n';
	import NomosMark from '$lib/components/nomos-mark.svelte';
	import { AssistantTurn, UserTurn } from '$lib/components/thread';
	import type { Message } from '$lib/api/conversations';
	import type { RunView, StepView } from '$lib/stores/conversation';
	import type { Artifact, RunQuestion } from '$lib/api/runs';

	/**
	 * The landing's product shot: the real thread components driven by a
	 * scripted run — the reply streams in, the plan assembles, pauses on a
	 * clarification, then delivers the bordereau — and loops. Inert and
	 * hidden from assistive tech: it is a picture of the product, not a form.
	 * Reduced motion shows the finished turn, still.
	 */

	const now = () => Math.floor(Date.now() / 1000);

	function userMessage(): Message {
		return {
			id: 'demo-user',
			tenant_id: '',
			conversation_id: 'demo',
			role: 'user',
			content: $t('landing.demo.ask'),
			run_id: null,
			state: 'complete',
			created_at: now(),
			updated_at: now()
		};
	}

	function step(key: string, kind: string, depends: string[] = []): StepView {
		return {
			id: key,
			step_key: key,
			kind,
			label: $t(`landing.demo.steps.${key.split('_')[0]}`),
			state: 'queued',
			attempt: 1,
			max_attempts: 3,
			depends_on: depends
		};
	}

	function artifact(id: string, kind: string, name: string): Artifact {
		return {
			id,
			tenant_id: '',
			run_id: 'demo-run',
			dossier_id: '',
			kind,
			asset_id: '',
			name,
			char_count: 0,
			template_version: kind === 'bordereau_docx' ? 'v1' : '',
			renderer_version: '',
			approved: false,
			approved_by: '',
			approved_at: null,
			created_at: now()
		};
	}

	let user = $state<Message>(userMessage());
	let reply = $state<Message>({ ...userMessage(), id: 'demo-reply', role: 'assistant', content: '' });
	let run = $state<RunView | null>(null);

	let timers: ReturnType<typeof setTimeout>[] = [];
	const wait = (ms: number) =>
		new Promise<void>((resolve) => timers.push(setTimeout(resolve, ms)));

	function finalState() {
		user = userMessage();
		reply = { ...userMessage(), id: 'demo-reply', role: 'assistant', content: $t('landing.demo.reply'), run_id: 'demo-run' };
		const steps = [
			step('extract_text', 'ocr'),
			step('qualify_piece', 'llm', ['extract_text']),
			step('clarify_ask_user', 'db', ['qualify_piece']),
			step('classement_build', 'db', ['clarify_ask_user']),
			step('export_dossier', 'render', ['classement_build'])
		].map((s) => ({ ...s, state: 'succeeded' as const }));
		run = {
			id: 'demo-run',
			state: 'succeeded',
			workflow_type: 'dossier_organize_v1',
			workflow_version: 1,
			steps,
			questions: [question('answered')],
			artifacts: [
				artifact('a1', 'bordereau_docx', $t('landing.demo.bordereau')),
				artifact('a2', 'export_zip', $t('landing.demo.zip'))
			],
			planned: true,
			created_at: now(),
			cancel_requested: false
		};
	}

	function question(state: 'pending' | 'answered'): RunQuestion {
		return {
			id: 'q1',
			tenant_id: '',
			run_id: 'demo-run',
			run_step_id: 'clarify_ask_user',
			question_key: 'matter',
			prompt: $t('landing.demo.question'),
			kind: 'single',
			options: ['travail', 'commercial', 'civil'].map((value) => ({
				value,
				label: $t(`landing.demo.options.${value}`)
			})),
			state,
			answer: state === 'answered' ? { values: ['travail'] } : undefined,
			answered_by: '',
			asked_at: now(),
			answered_at: state === 'answered' ? now() : null
		};
	}

	function setStep(index: number, patch: Partial<StepView>) {
		if (!run) return;
		run.steps[index] = { ...run.steps[index], ...patch };
	}

	async function play() {
		// 1. The ask lands; Nomos thinks.
		user = userMessage();
		run = null;
		reply = { ...userMessage(), id: 'demo-reply', role: 'assistant', content: '', state: 'streaming' };
		await wait(1700);

		// 2. The reply streams in bursts, as deltas do.
		const words = $t('landing.demo.reply').split(' ');
		for (let i = 0; i < words.length; ) {
			const burst = 1 + Math.floor(Math.random() * 4);
			reply.content = words.slice(0, (i += burst)).join(' ');
			await wait(70 + Math.random() * 120);
		}
		reply = { ...reply, content: $t('landing.demo.reply'), state: 'complete', run_id: 'demo-run' };
		await wait(500);

		// 3. The plan assembles and runs.
		run = {
			id: 'demo-run',
			state: 'running',
			workflow_type: 'dossier_organize_v1',
			workflow_version: 1,
			steps: [
				step('extract_text', 'ocr'),
				step('qualify_piece', 'llm', ['extract_text']),
				step('clarify_ask_user', 'db', ['qualify_piece']),
				step('classement_build', 'db', ['clarify_ask_user']),
				step('export_dossier', 'render', ['classement_build'])
			],
			questions: [],
			artifacts: [],
			planned: true,
			created_at: now(),
			cancel_requested: false
		};
		for (const index of [0, 1]) {
			setStep(index, { state: 'running', progress: 0 });
			for (let k = 1; k <= 10; k++) {
				await wait(150);
				setStep(index, { progress: k / 10 });
			}
			setStep(index, { state: 'succeeded', progress: undefined });
		}

		// 4. It stops to ask the question that changes the strategy.
		setStep(2, { state: 'waiting_input' });
		run!.state = 'waiting_input';
		run!.questions = [question('pending')];
		await wait(2600);
		run!.questions = [question('answered')];
		setStep(2, { state: 'succeeded' });
		run!.state = 'running';

		// 5. Classement, then the deliverables.
		for (const index of [3, 4]) {
			setStep(index, { state: 'running' });
			await wait(1200);
			setStep(index, { state: 'succeeded' });
		}
		run!.state = 'succeeded';
		run!.artifacts = [artifact('a1', 'bordereau_docx', $t('landing.demo.bordereau'))];
		await wait(160);
		run!.artifacts = [...run!.artifacts, artifact('a2', 'export_zip', $t('landing.demo.zip'))];

		await wait(6000);
		void play();
	}

	onMount(() => {
		if (prefersReducedMotion.current) finalState();
		else void play();
		return () => timers.forEach(clearTimeout);
	});
</script>

<div class="bg-card overflow-hidden rounded-[28px] border shadow-2xl" inert aria-hidden="true">
	<div class="border-border flex h-12 items-center gap-2.5 border-b px-5">
		<NomosMark size={18} />
		<span class="text-foreground truncate text-ui font-medium">{$t('landing.demo.threadTitle')}</span>
		<span class="bg-border h-3.5 w-px" aria-hidden="true"></span>
		<span class="bg-muted/70 text-foreground inline-flex h-6 items-center gap-1.5 rounded-md px-2 text-xs">
			<span class="font-mono text-[10px] text-muted-foreground tabular-nums">18</span>
			{$t('pieces.title').toLowerCase()}
		</span>
		<span class="ml-auto flex gap-1.5" aria-hidden="true">
			<span class="bg-border h-2 w-2 rounded-full"></span>
			<span class="bg-border h-2 w-2 rounded-full"></span>
			<span class="bg-border h-2 w-2 rounded-full"></span>
		</span>
	</div>
	<div class="h-[560px] space-y-7 overflow-hidden px-6 py-6 sm:px-8 [mask-image:linear-gradient(to_bottom,black_85%,transparent)]">
		<UserTurn message={user} />
		<AssistantTurn message={reply} {run} artifactsDownloadable={false} />
	</div>
</div>
