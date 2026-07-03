-- cx-review.lua — in-neovim review UI for cx doc edits.
-- Written by cx to ~/.local/share/cx/cx-review.lua on editor launch; do not edit.
--
-- Protocol: cx writes edits.json {file, edits:[{search, replace}]}, then calls
-- CxReview() over --remote. Each hunk renders as an inline diff (old lines
-- red, proposed lines green virtual text). Keys: y apply, n skip, N reject
-- with a note, a apply all, q quit. Decisions land in edits-done.json.

local ns = vim.api.nvim_create_namespace("cx_review")
local datadir = vim.fn.expand("~/.local/share/cx")

local S = { edits = nil, idx = 0, results = {}, buf = nil }

local function lines_of(s)
  return vim.split(s, "\n", { plain = true })
end

-- locate returns the 0-based start line where search_lines match, or nil.
-- Falls back to comparing with trailing whitespace stripped.
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

local function clear_marks()
  if S.buf and vim.api.nvim_buf_is_valid(S.buf) then
    vim.api.nvim_buf_clear_namespace(S.buf, ns, 0, -1)
  end
end

local function finish()
  clear_marks()
  for _, lhs in ipairs({ "y", "n", "N", "a", "q" }) do
    pcall(vim.keymap.del, "n", lhs, { buffer = S.buf })
  end
  vim.api.nvim_buf_call(S.buf, function()
    vim.cmd("silent! write")
  end)
  vim.fn.writefile({ vim.json.encode({ results = S.results }) }, datadir .. "/edits-done.json")
  local applied = 0
  for _, r in ipairs(S.results) do
    if r.applied then applied = applied + 1 end
  end
  vim.notify(string.format("cx: review done — %d/%d applied", applied, #S.results))
  S.edits = nil
end

local function show_current()
  clear_marks()
  if S.idx > #S.edits then
    finish()
    return
  end
  local e = S.edits[S.idx]
  local search = lines_of(e.search)
  local start = locate(S.buf, search)
  if not start then
    S.results[S.idx] = { applied = false, reason = "not found in buffer" }
    S.idx = S.idx + 1
    show_current()
    return
  end
  e._start, e._len = start, #search

  -- old lines: red block
  vim.api.nvim_buf_set_extmark(S.buf, ns, start, 0, {
    end_row = start + #search,
    hl_group = "DiffDelete",
    hl_eol = true,
  })
  -- proposed lines: green virtual block below, plus a control hint
  local virt = {}
  for _, l in ipairs(lines_of(e.replace)) do
    table.insert(virt, { { "+ " .. l, "DiffAdd" } })
  end
  table.insert(virt, {
    {
      string.format("── cx edit %d/%d ── y apply · n skip · N reject+note · a apply all · q quit", S.idx, #S.edits),
      "Comment",
    },
  })
  vim.api.nvim_buf_set_extmark(S.buf, ns, start + #search - 1, 0, { virt_lines = virt })

  local win = vim.fn.bufwinid(S.buf)
  if win ~= -1 then
    vim.api.nvim_win_set_cursor(win, { start + 1, 0 })
  end
end

local function apply_current()
  local e = S.edits[S.idx]
  vim.api.nvim_buf_set_lines(S.buf, e._start, e._start + e._len, false, lines_of(e.replace))
  vim.api.nvim_buf_call(S.buf, function()
    vim.cmd("silent! write")
  end)
  S.results[S.idx] = { applied = true }
end

local function decide(action)
  if not S.edits then
    return
  end
  if action == "apply" then
    apply_current()
    S.idx = S.idx + 1
    show_current()
  elseif action == "skip" then
    S.results[S.idx] = { applied = false }
    S.idx = S.idx + 1
    show_current()
  elseif action == "reject" then
    vim.ui.input({ prompt = "cx — why reject? " }, function(reason)
      S.results[S.idx] = { applied = false, reason = reason or "" }
      S.idx = S.idx + 1
      show_current()
    end)
  elseif action == "all" then
    while S.idx <= #S.edits do
      local e = S.edits[S.idx]
      local search = lines_of(e.search)
      local start = locate(S.buf, search)
      if start then
        e._start, e._len = start, #search
        apply_current()
      else
        S.results[S.idx] = { applied = false, reason = "not found in buffer" }
      end
      S.idx = S.idx + 1
    end
    finish()
  elseif action == "quit" then
    for i = S.idx, #S.edits do
      S.results[i] = { applied = false }
    end
    S.idx = #S.edits + 1
    finish()
  end
end

-- CxChecktime: cx pokes this after writing the file so the buffer hot-reloads.
function CxChecktime()
  vim.cmd("silent! checktime")
  return 1
end

-- CxReview: entry point, called by cx over --remote after writing edits.json.
function CxReview()
  local ok, raw = pcall(vim.fn.readfile, datadir .. "/edits.json")
  if not ok or #raw == 0 then
    return 0
  end
  local req = vim.json.decode(table.concat(raw, "\n"))
  if not req or not req.edits or #req.edits == 0 then
    return 0
  end

  -- Focus (or open) the target file
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
  S.edits = req.edits
  S.idx = 1
  S.results = {}

  for lhs, action in pairs({ y = "apply", n = "skip", N = "reject", a = "all", q = "quit" }) do
    vim.keymap.set("n", lhs, function()
      decide(action)
    end, { buffer = buf, nowait = true })
  end

  show_current()
  return 1
end
