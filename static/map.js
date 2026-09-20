(() => {
  const typing = el => el && (el.matches('input, textarea, select') || el.isContentEditable);
  const dialog = document.querySelector('[data-map-dialog]');
  const search = document.getElementById('map-search');
  document.querySelector('[role="alert"][tabindex="-1"]')?.focus();
  let helpOrigin, pendingGUntil = 0;
  const help = () => { helpOrigin = document.activeElement; dialog?.showModal(); };
  dialog?.addEventListener('close', () => helpOrigin?.focus());
  const move = delta => {
    const rows = [...document.querySelectorAll('[data-map-row]')].filter(row => row.getClientRects().length);
    if (rows.length) rows[Math.max(0, Math.min(rows.length - 1, rows.indexOf(document.activeElement) + delta))].focus();
  };
  document.querySelectorAll('[data-map-item-form]').forEach(form => {
    const select = form.querySelector('[data-map-kind-select]');
    if (!select) return;
    const update = () => form.querySelectorAll('[data-map-kind]').forEach(group => {
      group.hidden = group.dataset.mapKind !== select.value;
      group.disabled = group.hidden;
    });
    select.addEventListener('change', update); update();
  });

  const buttonClass = 'block min-h-11 w-full break-words border-b border-[#C8C3B5] px-3 py-2 text-left text-sm hover:bg-[#ECE6D6] focus:outline focus:outline-2';
  document.querySelectorAll('[data-map-source]').forEach(source => {
    const input = source.querySelector('[data-map-pr-search]'), id = source.querySelector('[data-map-pr-id]');
    const results = source.querySelector('[data-map-pr-results]'), selection = source.querySelector('[data-map-pr-selection]');
    const urlInput = source.querySelector('[name="source_url"]');
    if (!input || !id || !results || !selection) return;
    let controller, query = '', timer, generation = 0;
    const clear = () => { id.value = ''; selection.textContent = 'No cached PR selected.'; };
    const button = (label, click) => { const el = document.createElement('button'); el.type = 'button'; el.className = buttonClass; el.textContent = label; el.addEventListener('click', click); return el; };
    selection.after(button('Clear cached PR selection', clear));
    urlInput?.addEventListener('input', clear);
    const load = async (cursor = '') => {
      controller?.abort(); controller = new AbortController();
      const requested = ++generation;
      results.textContent = 'Loading cached PRs…';
      try {
        const response = await fetch('/map/prs?q=' + encodeURIComponent(query) + '&limit=20&cursor=' + encodeURIComponent(cursor), {signal: controller.signal});
        if (!response.ok) throw new Error('Unavailable');
        const page = await response.json();
        if (requested !== generation) return;
        results.replaceChildren();
        const rows = Array.isArray(page.Rows) ? page.Rows : [];
        if (!rows.length) results.textContent = 'No matching cached PRs. You can paste a source URL instead.';
        rows.forEach(pr => {
          const observed = pr.LastObservedAt ? new Date(pr.LastObservedAt).toLocaleString() : 'unknown';
          const label = pr.Owner + '/' + pr.Repo + '#' + pr.Number + ' · ' + pr.Title + ' · cached ' + pr.State + ' · last observed ' + observed;
          results.append(button(label, () => { id.value = String(pr.ID); if (urlInput) urlInput.value = pr.URL; selection.textContent = 'Selected cached PR: ' + label; results.replaceChildren(); }));
        });
        if (page.NextCursor) results.append(button('Next cached PR page', () => load(page.NextCursor)));
        if (cursor) results.append(button('First cached PR page', () => load()));
      } catch (error) { if (error.name !== 'AbortError' && requested === generation) results.textContent = 'Cached PR search is unavailable. Try again.'; }
    };
    input.addEventListener('input', () => {
      clearTimeout(timer); controller?.abort(); generation++;
      query = input.value.trim(); results.replaceChildren();
      if (query) timer = setTimeout(() => load(), 150);
    });
  });

  document.querySelectorAll('[data-map-destination]').forEach(container => {
    const input = container.querySelector('[data-map-destination-search]');
    const select = container.querySelector('[name="destination_workstream_id"]');
    const status = container.querySelector('[data-map-destination-status]');
    if (!input || !select) return;
    let controller, generation = 0;
    input.addEventListener('input', async () => {
      controller?.abort(); controller = new AbortController(); const requested = ++generation;
      try {
        const response = await fetch('/map/data?filter=active&q=' + encodeURIComponent(input.value), {signal: controller.signal});
        if (!response.ok) throw new Error('Unavailable');
        const page = await response.json(); if (requested !== generation) return;
        select.replaceChildren(new Option('Choose an active workstream', ''));
        (page.Workstreams.Rows || []).filter(row => row.ID !== container.dataset.mapCurrentWorkstream).forEach(row => select.add(new Option(row.Name, row.ID)));
        if (status) status.textContent = page.Workstreams.Total > 50 ? 'First 50 matches. Narrow your search to find the destination.' : (select.options.length - 1) + ' active destinations.';
      } catch (error) { if (error.name !== 'AbortError' && requested === generation && status) status.textContent = 'Destination search unavailable. Try again.'; }
    });
  });

  document.addEventListener('click', event => { if (event.target.closest('[data-map-help]')) help(); });
  document.addEventListener('submit', event => {
    if (event.target.method !== 'post' || !event.submitter) return;
    event.submitter.disabled = true; event.submitter.textContent = 'Saving…';
  });
  document.addEventListener('keydown', event => {
    if (event.isComposing || event.ctrlKey || event.metaKey || event.altKey || typing(event.target) || dialog?.open) return;
    if (event.key === '?') { event.preventDefault(); help(); return; }
    if (event.key === '/') { event.preventDefault(); search?.focus(); return; }
    if (event.key === 'j') { event.preventDefault(); move(1); return; }
    if (event.key === 'k') { event.preventDefault(); move(-1); return; }
    if (event.key === 'e' && document.activeElement?.matches('[data-map-row]')) { event.preventDefault(); document.activeElement.click(); return; }
    if (performance.now() < pendingGUntil && event.key === 'w') { event.preventDefault(); pendingGUntil = 0; document.getElementById('map-workstreams')?.focus(); return; }
    pendingGUntil = event.key === 'g' ? performance.now() + 1000 : 0;
  });
})();
