<script lang="ts">
  import { t } from '$lib/i18n/index.svelte';
  let { text, cols = 1 }: { text?: string; cols?: number } = $props();
</script>

<tr><td colspan={cols} class="text-muted text-sm py-6 text-center">{text ?? t('shell.empty.default')}</td></tr>
