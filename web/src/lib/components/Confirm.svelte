<script lang="ts" module>
  // Одно окно подтверждения на всю панель. Кнопка в строке таблицы — это
  // иконка без подписи; человек должен узнать, что именно сейчас произойдёт,
  // до того как это произойдёт, а не из тоста после.
  export type Ask = {
    title: string;
    /** Что случится на самом деле: последствия, а не пересказ заголовка. */
    note?: string;
    /** Подпись кнопки действия: глагол, тот же что и в заголовке. */
    action?: string;
    danger?: boolean;
    run: () => unknown;
  };
</script>

<script lang="ts">
  import type { Snippet } from 'svelte';
  import Modal from './Modal.svelte';

  let { ask = $bindable(null), children }: { ask?: Ask | null; children?: Snippet } = $props();
  let busy = $state(false);

  async function go() {
    if (!ask || busy) return;
    const run = ask.run;
    busy = true;
    try {
      await run();
      ask = null;
    } finally {
      busy = false;
    }
  }
</script>

<Modal open={!!ask} title={ask?.title ?? ''} onclose={() => (ask = null)}>
  {#if ask?.note}<p class="text-muted">{ask.note}</p>{/if}
  {@render children?.()}
  {#snippet footer()}
    <button class="btn" onclick={() => (ask = null)} disabled={busy}>Отмена</button>
    <button class="btn {ask?.danger ? 'btn-danger' : 'btn-primary'}" onclick={go} disabled={busy}>
      {busy ? 'выполняю…' : (ask?.action ?? 'Продолжить')}
    </button>
  {/snippet}
</Modal>
