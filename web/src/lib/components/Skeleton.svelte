<script lang="ts">
  let { rows = 4 }: { rows?: number } = $props();
</script>

<div class="space-y-2 p-4">
  {#each Array(rows) as _, i}
    <div class="skel h-6" style="width: {88 - (i % 3) * 14}%"></div>
  {/each}
</div>
