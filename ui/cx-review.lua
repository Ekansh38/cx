-- cx-review.lua — in-neovim review UI for cx doc edits.
-- Written by cx to ~/.local/share/cx/cx-review.lua on editor launch; do not edit.
--
-- Proposed text is inserted as REAL buffer lines (green) below the lines it
-- replaces (red). Decisions are plain buffer edits: y deletes the red block
-- (applies), n/N deletes the green block (skips/rejects), a applies all,
-- q finishes. There is NO custom undo: plain vim u reverts a decision, the
-- highlights restore with the text (extmark undo_restore), and a TextChanged
-- autocmd repaints the footer/quickfix so the hunk is visibly pending again.
--
-- Hunk state is DERIVED from the buffer: red gone = applied, green gone =
-- skipped, both present = pending. The file is written when the review
-- finishes; results land in edits-done.json for cx.

local ns = vim.api.nvim_create_namespace("cx_review")
local aug = vim.api.nvim_create_augroup("cx_review", { clear = true })
local datadir = vim.fn.expand("~/.local/share/cx")

local S = { hunks = nil, buf = nil, total = 0 }

local function lines_of(s)
  return vim.split(s, "\n", { plain = true })
end

local function locate(buf, search_lines)
  local total = vim.api.nvim_buf_line_count(buf)
  local n = #search_lines
  local function try(norm)
    for i = 0, total - n do
      local chunk = vim.api.nvim_buf_get_lines(buf, i, i + n, false)
      local match = true
      for j = 1, n do
        local a, b = chunk[j], search_lines[j]
        if norm then
          a, b = a:gsub("%s+$", ""), b:gsub("%s+$", "")
        end
        if a ~= b then
          match = false
          break
        end
      end
      if match then
        return i
      end
    end
    return nil
  end
  return try(false) or try(true)
end

local function common_affixes(a, b)
  local p = 0
  local maxp = math.min(#a, #b)
  while p < maxp and a[p + 1] == b[p + 1] do
    p = p + 1
  end
  local sfx = 0
  while sfx < math.min(#a, #b) - p and a[#a - sfx] == b[#b - sfx] do
    sfx = sfx + 1
  end
  return p, sfx
end

-- split_diff: changed byte span between two single ASCII lines, snapped to
-- word boundaries.
local function split_diff(old, new)
  local p = 0
  local maxp = math.min(#old, #new)
  while p < maxp and old:byte(p + 1) == new:byte(p + 1) do
    p = p + 1
  end
  local oe, ne = #old, #new
  while oe > p and ne > p and old:byte(oe) == new:byte(ne) do
    oe = oe - 1
    ne = ne - 1
  end
  while p > 0 and old:sub(p, p) ~= " " do
    p = p - 1
  end
  while oe < #old and ne < #new and old:sub(oe + 1, oe + 1) ~= " " do
    oe = oe + 1
    ne = ne + 1
  end
  return p, oe, ne
end

local function is_ascii(s)
  return not s:find("[\128-\255]")
end

-- mark_info returns {startRow, endRowExclusive} for a live extmark, or nil
-- when the mark is gone/invalidated (its text was deleted).
local function mark_info(id)
  if not id then
    return nil
  end
  local m = vim.api.nvim_buf_get_extmark_by_id(S.buf, ns, id, { details = true })
  if not m or #m == 0 then
    return nil
  end
  if m[3] and m[3].invalid then
    return nil
  end
  local endRow = (m[3] and m[3].end_row) or m[1]
  if endRow <= m[1] then
    endRow = m[1] + 1
  end
  return { m[1], endRow }
end

-- state derives a hunk's status from what is actually in the buffer.
local function state(h)
  if h.notfound then
    return "notfound"
  end
  local red = h.delN > 0 and mark_info(h.redMark) ~= nil
  local green = h.insN > 0 and mark_info(h.greenMark) ~= nil
  if h.delN > 0 and h.insN > 0 then
    if red and green then
      return "pending"
    elseif green then
      return "applied"
    end
    return "skipped"
  elseif h.delN == 0 then -- pure insertion
    if not green then
      return "skipped"
    elseif h.accepted then
      return "applied"
    end
    return "pending"
  else -- pure deletion
    if not red then
      return "applied"
    elseif h.skipped then
      return "skipped"
    end
    return "pending"
  end
end

local function pending()
  local n = 0
  for _, h in ipairs(S.hunks) do
    if state(h) == "pending" then
      n = n + 1
    end
  end
  return n
end

local function hunk_row(h)
  local r = mark_info(h.redMark) or mark_info(h.greenMark)
  return r and r[1] or nil
end

-- repaint refreshes footers and the quickfix list from derived state. Runs
-- after decisions AND on TextChanged, so vim-native undo brings the full
-- review UX back.
local function repaint()
  if not S.hunks then
    return
  end
  local qf = {}
  for i, h in ipairs(S.hunks) do
    local st = state(h)
    if h.footMark then
      pcall(vim.api.nvim_buf_del_extmark, S.buf, ns, h.footMark)
      h.footMark = nil
    end
    if st == "pending" then
      local row = hunk_row(h)
      if row then
        local r = mark_info(h.greenMark) or mark_info(h.redMark)
        h.footMark = vim.api.nvim_buf_set_extmark(S.buf, ns, r[2] - 1, 0, {
          virt_lines = {
            {
              {
                string.format("cx %d/%d · y apply  n skip  N reject+note  a all  q finish  (u = vim undo)", i, S.total),
                "Comment",
              },
            },
          },
        })
        table.insert(qf, {
          bufnr = S.buf,
          lnum = row + 1,
          text = string.format("cx edit %d/%d", i, S.total),
        })
      end
    end
  end
  pcall(vim.fn.setqflist, {}, " ", { title = "cx edits", items = qf })
end

local function covers(id, line)
  local r = mark_info(id)
  return r ~= nil and line >= r[1] + 1 and line <= r[2]
end

local function hunk_at(line)
  for _, h in ipairs(S.hunks or {}) do
    if state(h) == "pending" and (covers(h.redMark, line) or covers(h.greenMark, line)) then
      return h
    end
  end
  return nil
end

local function current_hunk()
  local win = vim.fn.bufwinid(S.buf)
  if win == -1 then
    return nil
  end
  return hunk_at(vim.api.nvim_win_get_cursor(win)[1])
end

local function goto_next_pending()
  local win = vim.fn.bufwinid(S.buf)
  if win == -1 then
    return
  end
  if hunk_at(vim.api.nvim_win_get_cursor(win)[1]) then
    return
  end
  for _, h in ipairs(S.hunks) do
    if state(h) == "pending" then
      local row = hunk_row(h)
      if row then
        vim.api.nvim_win_set_cursor(win, { row + 1, 0 })
        return
      end
    end
  end
end

local function finish()
  local results, applied = {}, 0
  for _, h in ipairs(S.hunks) do
    local st = state(h)
    local r
    if st == "applied" then
      r = { applied = true }
      applied = applied + 1
    elseif st == "notfound" then
      r = { applied = false, reason = "not found in buffer" }
    else
      r = { applied = false }
      if h.reason and h.reason ~= "" then
        r.reason = h.reason
        r.reported = h.reported or nil
      end
    end
    table.insert(results, r)
  end
  vim.api.nvim_buf_clear_namespace(S.buf, ns, 0, -1)
  vim.api.nvim_clear_autocmds({ group = aug, buffer = S.buf })
  for _, lhs in ipairs({ "y", "n", "N", "a", "q" }) do
    pcall(vim.keymap.del, "n", lhs, { buffer = S.buf })
  end
  pcall(vim.api.nvim_buf_call, S.buf, function()
    vim.cmd("silent! write")
  end)
  pcall(vim.fn.setqflist, {}, "r")
  vim.fn.writefile({ vim.json.encode({ results = results }) }, datadir .. "/edits-done.json")
  vim.notify(string.format("cx: %d/%d applied", applied, #results))
  S.hunks = nil
end

local function delete_block(id)
  local r = mark_info(id)
  if r then
    vim.api.nvim_buf_set_lines(S.buf, r[1], r[2], false, {})
  end
end

local function post_decide()
  repaint()
  if pending() == 0 then
    finish()
  else
    goto_next_pending()
  end
end

local function do_apply(h)
  if h.delN > 0 then
    delete_block(h.redMark)
  else
    h.accepted = true
  end
  h.reason = nil
end

local function do_skip(h)
  if h.insN > 0 then
    delete_block(h.greenMark)
  else
    h.skipped = true
  end
end

local function decide(action)
  if not S.hunks then
    return
  end
  if action == "all" then
    for _, h in ipairs(S.hunks) do
      if state(h) == "pending" then
        do_apply(h)
      end
    end
    post_decide()
    return
  end
  if action == "quit" then
    for _, h in ipairs(S.hunks) do
      if state(h) == "pending" then
        do_skip(h)
      end
    end
    finish()
    return
  end

  local h = current_hunk()
  if not h then
    vim.notify("cx: move the cursor onto an edit (]q)")
    return
  end
  if action == "apply" then
    do_apply(h)
    post_decide()
  elseif action == "skip" then
    do_skip(h)
    post_decide()
  elseif action == "reject" then
    vim.ui.input({ prompt = "why? " }, function(reason)
      do_skip(h)
      h.reason = reason or ""
      if reason and reason ~= "" then
        h.reported = true
        local f = io.open(datadir .. "/reject-now.jsonl", "a")
        if f then
          f:write(vim.json.encode({
            reason = reason,
            search = table.concat(h.search, "\n"),
            replace = table.concat(h.replace, "\n"),
          }) .. "\n")
          f:close()
        end
      end
      post_decide()
    end)
  end
end

-- decorate paints one hunk's red/green highlights. invalidate=true means the
-- marks report gone when their block is deleted; undo_restore (default)
-- brings them back when vim undo restores the text.
local function decorate(h, oldStart, insStart)
  if h.word then
    local p, oe, ne = split_diff(h.old[1], h.new[1])
    if oe > p then
      h.redMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, p, {
        end_col = oe,
        hl_group = "DiffDelete",
        invalidate = true,
      })
    else
      h.redMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, 0, {
        end_row = oldStart + 1,
        hl_group = "DiffDelete",
        hl_eol = true,
        invalidate = true,
      })
    end
    if ne > p then
      h.greenMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, p, {
        end_col = ne,
        hl_group = "DiffAdd",
        invalidate = true,
      })
    else
      h.greenMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, 0, {
        end_row = insStart + 1,
        hl_group = "DiffAdd",
        hl_eol = true,
        invalidate = true,
      })
    end
  else
    if h.delN > 0 then
      h.redMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, 0, {
        end_row = oldStart + h.delN,
        hl_group = "DiffDelete",
        hl_eol = true,
        invalidate = true,
      })
    end
    if h.insN > 0 then
      h.greenMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, 0, {
        end_row = insStart + h.insN,
        hl_group = "DiffAdd",
        hl_eol = true,
        invalidate = true,
      })
    end
  end
end

function CxChecktime()
  vim.cmd("silent! checktime")
  return 1
end

function CxReview()
  local ok, raw = pcall(vim.fn.readfile, datadir .. "/edits.json")
  if not ok or #raw == 0 then
    return 0
  end
  local req = vim.json.decode(table.concat(raw, "\n"))
  if not req or not req.edits or #req.edits == 0 then
    return 0
  end

  local buf = vim.fn.bufnr(req.file)
  if buf == -1 then
    vim.cmd("edit " .. vim.fn.fnameescape(req.file))
    buf = vim.api.nvim_get_current_buf()
  else
    local win = vim.fn.bufwinid(buf)
    if win ~= -1 then
      vim.api.nvim_set_current_win(win)
    else
      vim.api.nvim_set_current_buf(buf)
    end
  end
  vim.cmd("silent! checktime")

  S.buf = buf
  S.total = #req.edits
  S.hunks = {}

  for _, e in ipairs(req.edits) do
    local h = { search = lines_of(e.search), replace = lines_of(e.replace) }
    h.start = locate(buf, h.search)
    if not h.start then
      h.notfound = true
    else
      local p, sfx = common_affixes(h.search, h.replace)
      h.p = p
      h.delN = #h.search - p - sfx
      h.insN = #h.replace - p - sfx
      h.old = {}
      for k = p + 1, #h.search - sfx do
        table.insert(h.old, h.search[k])
      end
      h.new = {}
      for k = p + 1, #h.replace - sfx do
        table.insert(h.new, h.replace[k])
      end
      h.word = h.delN == 1 and h.insN == 1
        and is_ascii(h.old[1] or "") and is_ascii(h.new[1] or "")
    end
    table.insert(S.hunks, h)
  end

  -- insert green blocks bottom-up so earlier positions stay valid
  local order = {}
  for i, h in ipairs(S.hunks) do
    if not h.notfound then
      table.insert(order, i)
    end
  end
  table.sort(order, function(a, b)
    return S.hunks[a].start > S.hunks[b].start
  end)
  for _, i in ipairs(order) do
    local h = S.hunks[i]
    local oldStart = h.start + h.p
    local insStart = oldStart + h.delN
    if #h.new > 0 then
      vim.api.nvim_buf_set_lines(buf, insStart, insStart, false, h.new)
    end
    decorate(h, oldStart, insStart)
  end

  repaint()
  if pending() == 0 then
    finish()
    return 1
  end
  goto_next_pending()

  for lhs, action in pairs({ y = "apply", n = "skip", N = "reject", a = "all", q = "quit" }) do
    vim.keymap.set("n", lhs, function()
      decide(action)
    end, { buffer = buf, nowait = true })
  end

  -- vim-native undo/redo (or any edit) repaints the review state
  vim.api.nvim_clear_autocmds({ group = aug, buffer = buf })
  vim.api.nvim_create_autocmd({ "TextChanged", "TextChangedI" }, {
    group = aug,
    buffer = buf,
    callback = function()
      if S.hunks then
        repaint()
      end
    end,
  })

  local word = S.total == 1 and "edit" or "edits"
  vim.notify(string.format("cx: %d %s · y/n/N/a/q · u = vim undo · ]q next", S.total, word))
  return 1
end
