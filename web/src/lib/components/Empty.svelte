<script lang="ts">
  let { text = 'Пока пусто.', cols = 1 }: { text?: string; cols?: number } = $props();
</script>

<tr><td colspan={cols} class="text-muted text-sm py-6 text-center">{text}</td></tr>
