# ozemflac

Music library converter that mirrors a source directory to a destination.

Target use case: convert a large lossless music library into a smaller and lossy library for phone, laptop, etc., ignoring some files and doing cover optimization.

Lossless sources (`.flac`, `.alac`) and ALAC `.m4a` are transcoded to the selected preset, lossy sources (`.mp3`, `.aac`, `.ogg`, `.opus`) and AAC `.m4a` are copied, cover images are copied only if the subtree contains music.

Per-folder presets via `.ozemrc` and ignores via `.ozemignore` (gitignore syntax, stacked) are respected. `tree`/`diff`/`sync` provide inspection and incremental update.

Converted files keep the source metadata and original base name with the preset extension (`song.m4a (ALAC) → .m4a` is disambiguated).

## Prerequisites

- Go 1.25.5 (`go.mod`)
- `ffmpeg` and `ffprobe` in `$PATH` (always required; `convert`/`sync` fail fast if missing)
- Linux: `ffmpeg` built with `--enable-libfdk-aac` (`libfdk_aac`) for AAC and `--enable-libmp3lame` (`libmp3lame`) for MP3. Checked via `ffmpeg -encoders` at runtime.
- macOS: `/usr/bin/afconvert` (Xcode Command Line Tools) for AAC, `ffmpeg` with `libmp3lame` for MP3; AAC metadata copy still uses `ffmpeg`.

## Commands

### `ozemflac` (convert)

```
ozemflac [-workers N] [-preset aac|mp3] <source_dir> <destination_dir>
```

Copies/converts the whole source tree to a new or empty destination. Fails if destination exists and is not empty, or if source and destination are the same.

| Flag       | Default                                | Description                                                                                                                                                                                                           |
| ---------- | -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | 
| `-preset`  | `aac`                                  | Output preset. `aac` → `.m4a` (256k, `libfdk_aac` on Linux / `afconvert` on macOS), `mp3` → `.mp3` (320k `libmp3lame`). Strict lower-case; invalid value lists available presets. Per-folder `.ozemrc` (`preset = aac | mp3`) overrides, inherited via ancestors. |
| `-workers` | `0` (auto `runtime.NumCPU()/2`, min 1) | Parallel conversion workers.                                                                                                                                                                                          |

Behavior: `scanSource` collects candidates (`losslessExt`, `lossyExt`, `coverFiles: cover.jpg/folder.jpg/cover.png/folder.png`, `.m4a` probed with `ffprobe -select_streams a:0 -show_entries stream=codec_name` → `alac` ? convert : copy). Covers skipped if `subtreeHasMusic[dir]==false`. Progress bar `█/-` with percent.

### `ozemflac tree`

```
ozemflac tree [--all] [--json] [--expand] [-preset aac|mp3] <source_dir>
```

Shows the source tree with effective preset, pruned/ignored info. No destination.

| Flag              | Default | Description                                                                                                                                                      |
| ----------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-preset`         | `aac`   | Same as convert, used as fallback for `effectivePresetFor`.                                                                                                      |
| `--all`           | `false` | Show ignored files (`[ignored]` dim/red) including inside pruned dirs.                                                                                           |
| `--json`          | `false` | JSON output (`path`, `preset`, `presetSource`, `pruned`, `stats{ music, covers, ignored, byExt }`, `files`, `ignoredFiles`, `children`). Disables spinner/color. |
| `--expand` / `-v` | `false` | Always expand file list; otherwise collapsed when the whole directory shares one preset/action.                                                                  |

Root line: `<src> [preset: aac → .m4a (default|via .ozemrc|inherited)] [ignores:N]`. Dir lines include `pruned (.ozemignore|empty)` and `music:X covers:Y`.

### `ozemflac diff`

```
ozemflac diff [--all] [--json] [--full] [--expand] [-preset aac|mp3] <source_dir> <destination_dir>
```

Two-column `SRC | DST [status]` adaptive view (`COLUMNS` or `TIOCGWINSZ` else 80, truncated with `…`, padded via `go-runewidth`). Compares expected dst (preset-aware, ALAC probing) against actual dst on disk.

Statuses (third column, same line, dynamic width `statusW`):

- `[mismatch]` yellow – same base, different ext (preset mismatch): `03 - Fat.flac → .m4a` vs `03 - Fat.mp3`
- `[missing]` green – expected not in dst
- `[extra]` red – in dst but not expected
- `[ignored]` dim/red with `--all`

| Flag              | Default | Description                                                                                                      |
| ----------------- | ------- | ---------------------------------------------------------------------------------------------------------------- |
| `-preset`         | `aac`   | Fallback preset for expected mapping.                                                                            |
| `--all`           | `false` | Show `ignoredFiles` as `[ignored]`.                                                                              |
| `--json`          | `false` | JSON `{source,destination,preset,missing[],extra[],mismatch[{expected,actual,src}],upToDate[]}` (when `--full`). |
| `--full`          | `false` | Show full tree including `upToDate` files, not only diffs.                                                       |
| `--expand` / `-v` | `false` | Expand file lists in `--full` mode even when uniform.                                                            |

Spinner phases: `scanning src` → `probing .m4a` → `scanning dst`.

### `ozemflac sync`

```
ozemflac sync [--all] [--json] [--full] [--expand] [--dry-run] [--keep-mismatched] [--delete-extra] [-workers N] [-preset aac|mp3] <source_dir> <destination_dir>
```

`diff` + apply. By default adds `missing` and replaces `mismatch`; keeps `extra`. After a real run prints a collapsed change view (whole folder replaced → single `album/ [preset:aac, synced N files] [synced]` line).

| Flag                | Default | Description                                                                                                                                              |
| ------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-preset`           | `aac`   | Same as `diff`/`convert`.                                                                                                                                |
| `-workers`          | `0`     | Same as `convert`. Only checked when a `convert` job is needed.                                                                                          |
| `--keep-mismatched` | `false` | When set, mismatched files are kept (skipped). Default replaces them: old ext file is removed then re-encoded/copied.                                    |
| `--delete-extra`    | `false` | When set, extra files in dst are deleted and empty parent dirs pruned. Default keeps them.                                                               |
| `--dry-run`         | `false` | Preview without mutating dst. Shows `Preview: N to add, N to replace, N to delete` or `Already in sync`.                                                 |
| `--json`            | `false` | JSON `{source,destination,preset,dryRun,added[],replaced[{expected,actual,src}],deleted[],keptMismatch[],keptExtra[]}`. Non-dry-run JSON skips the tree. |
| `--all`             | `false` | Show ignored files in preview/result tree (same `statusW` handling as `diff`).                                                                           |
| `--full`            | `false` | Show full tree including up-to-date files in the result view.                                                                                            |
| `--expand` / `-v`   | `false` | Expand uniform-preset file lists in the result view.                                                                                                     |

Execution order: `extra` deletions (deepest first) → `mismatch` old-file removal → `missing` + `mismatch` jobs via `processFile` (same tmp→rename safety as `convert`). Progress bar and `Done!`/`Done with N error(s)` match `convert`. Collapsing rule: if for a directory `synced == totalExpectedInSubtree` and no `upToDate` remains (and no undeleted extra in subtree), the directory is printed once as `[synced]` instead of per-file lines.
