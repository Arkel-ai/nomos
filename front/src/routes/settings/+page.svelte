<script lang="ts">
	// /settings — reached from the account menu. Three tabs: the Better Auth
	// account (profile, password, two-factor), the Nomos preferences
	// (language, style preferences, explicit memory), and the user's data
	// (erase their conversations and dossiers).
	import Icon from '$lib/components/icon.svelte';
	import * as Tabs from '$lib/components/ui/tabs';
	import MotionTabsList from '$lib/components/motion-tabs-list.svelte';
	import AccountSettings from '$lib/components/settings/account-settings.svelte';
	import PreferencesSettings from '$lib/components/settings/preferences-settings.svelte';
	import DataSettings from '$lib/components/settings/data-settings.svelte';
	import { reveal } from '$lib/motion';
	import { t } from '$lib/i18n';
</script>

<svelte:head>
	<title>{$t('settings.title')} · {$t('title')}</title>
</svelte:head>

<div use:reveal class="mx-auto w-full max-w-3xl space-y-6 pb-6">
	<header class="space-y-2">
		<h1 class="display text-foreground text-[40px]">{$t('settings.title')}</h1>
		<p class="text-muted-foreground text-sm">{$t('settings.subtitle')}</p>
	</header>

	<Tabs.Root value="account" class="gap-5">
		<MotionTabsList>
			<Tabs.Trigger value="account">
				<Icon name="user-rounded" class="h-4 w-4" />
				{$t('settings.tabAccount')}
			</Tabs.Trigger>
			<Tabs.Trigger value="preferences">
				<Icon name="tuning-2" class="h-4 w-4" />
				{$t('settings.tabPreferences')}
			</Tabs.Trigger>
			<Tabs.Trigger value="data">
				<Icon name="database" class="h-4 w-4" />
				{$t('settings.dataTitle')}
			</Tabs.Trigger>
		</MotionTabsList>
		<Tabs.Content value="account">
			<AccountSettings />
		</Tabs.Content>
		<Tabs.Content value="preferences">
			<PreferencesSettings />
		</Tabs.Content>
		<Tabs.Content value="data">
			<DataSettings />
		</Tabs.Content>
	</Tabs.Root>
</div>
