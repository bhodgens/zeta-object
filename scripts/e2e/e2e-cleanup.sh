#!/usr/bin/env bash
# e2e-cleanup.sh — the e2e suite's cleanup contract, in ONE sourceable file.
#
# WHY THIS IS A FILE AND NOT A FUNCTION INSIDE run-e2e.sh: both the suite and
# `make e2e-clean` must run it, and extracting a bash function out of a script
# with sed is fragile (it silently produced a half-defined function once, and
# the sweeper then reported success while removing nothing). Sourcing a real
# file makes the contract testable on its own.
#
# WHAT IT GUARDS: a bughunt on 2026-10-08 found the suite leaking on two axes.
#   1. FILES: cases 32/33 mktemp'd a body file per signed request OUTSIDE their
#      temp root and never removed them - 450 files accumulated in /tmp on the
#      dev machine. Those cases are fixed; this sweeper is the backstop for any
#      future case that repeats the mistake, and for runs interrupted before
#      the per-case trap fired.
#   2. PROCESSES: an interrupted run (Ctrl-C, a killed shell, a CI timeout)
#      skipped every per-case cleanup, leaving the suite server holding its
#      ports. One such server was found 6 days later still listening.
#
# SAFETY: it only touches this suite's own namespace (/tmp/e2e[0-9]*,
# /tmp/zetaobject-e2e.*, /tmp/zeta-one.*) and only server processes whose
# command line names this repo. E2E_SWEEP_MIN_AGE (seconds, default 30) keeps a
# CONCURRENTLY RUNNING suite out of harm's way; a live server with a listener is
# never reaped regardless. Set it to 0 only when you know nothing else runs.

# e2e_mtime prints a path's mtime in seconds (0 when unavailable). BSD stat
# (-f %m) and GNU stat (-c %Y) both appear because this harness runs on dev
# machines AND in CI containers.
e2e_mtime() {
	local v
	v=$(stat -f %m "$1" 2>/dev/null) || v=$(stat -c %Y "$1" 2>/dev/null) || v=0
	case "$v" in '' | *[!0-9]*) v=0 ;; esac
	printf '%s' "$v"
}

# e2e_sweep_stale removes stale temp state and orphaned servers.
# REPO_ROOT must be set by the caller; E2E_SWEEP_MIN_AGE is honored if set.
# e2e_proc_age prints a process's age in seconds, or nothing when ps cannot
# answer. Portability note: `ps -o etimes=` is GNU-only; on macOS/BSD it prints
# the ENTIRE field keyword list instead of a value, which is a silent failure
# that made the sweeper's age gate never fire (every orphan looked like it had
# no age at all, so it was skipped). BSD's `-o etime=` gives [[DD-]HH:]MM:SS,
# which is parsed here.
e2e_proc_age() {
	local pid="$1" raw days h m sec
	# Try GNU's seconds field first, but VALIDATE it: macOS answers that
	# request by printing the whole ps keyword list, which is a long string
	# that is not purely numeric. A `*[!0-9]*` test alone is not enough - the
	# keyword list contains digits - so the value is accepted only when the
	# output is a SINGLE line of digits.
	raw=$(ps -o etimes= -p "$pid" 2>/dev/null | head -1 | tr -d ' ')
	case "$raw" in
	'' | *[!0-9]*)
		raw=$(ps -o etime= -p "$pid" 2>/dev/null | head -1 | tr -d ' ')
		;;
	*)
		printf '%s' "$raw"
		return 0
		;;
	esac
	case "$raw" in
	*-*) days=${raw%%-*}; raw=${raw#*-} ;;
	*) days=0 ;;
	esac
	# normalise to HH:MM:SS
	case $(printf '%s' "$raw" | tr -cd ':' | wc -c | tr -d ' ') in
	1) raw="0:$raw" ;;
	esac
	# 10# prefixes are load-bearing: bash evaluates a leading-zero token as
	# OCTAL, so an etime of "09:18:21" failed with "value too great for base".
	#
	# The read MUST NOT run inside a subshell: a command-substitution group
	# scopes the assignments away, h/m/sec come back empty, and every age
	# silently parses as 0 - which is what made the sweeper skip every orphan.
	local IFS=:
	read -r h m sec <<<"$raw"
	IFS=$' 	\n'
	h=${h#0}; h=${h:-0}
	m=${m#0}; m=${m:-0}
	sec=${sec#0}; sec=${sec:-0}
	case "$days$h$m$sec" in
	'' | *[!0-9]*)
		printf ''
		return 0
		;;
	esac
	printf '%s' $(( 10#$days * 86400 + 10#$h * 3600 + 10#$m * 60 + 10#$sec ))
}

e2e_sweep_stale() {
	local age_cutoff="${E2E_SWEEP_MIN_AGE:-30}" now path mtime pid cmd ppid age
	now=$(date +%s)

	for path in /tmp/e2e[0-9]* /tmp/zetaobject-e2e.* /tmp/zeta-one.*; do
		[ -e "$path" ] || continue
		mtime=$(e2e_mtime "$path")
		[ "$mtime" -gt 0 ] || continue
		[ $((now - mtime)) -ge "$age_cutoff" ] || continue
		rm -rf "$path" 2>/dev/null || true
	done

	# Three gates, all of which must pass before a process is reaped:
	#   a. the command line names THIS repo or the suite's own relative
	#      invocation (a stranger's server never matches);
	#   b. it is ORPHANED - PPID 1, meaning the shell that launched it (a case's
	#      `bash -c`, or run-e2e.sh itself) is gone. A server belonging to a
	#      suite that is STILL RUNNING has a live parent shell and never
	#      matches.
	#      This replaced an earlier "has no LISTENing socket" gate, which was
	#      wrong in the exact case it was meant to catch: an orphaned server
	#      KEEPS its listener, so the gate never fired and 9-hour-old orphans
	#      survived every sweep.
	#   c. it is older than the cutoff, so a suite that started moments ago is
	#      never a candidate even if (b) is briefly untrue during startup.
	for pid in $(pgrep -f 'zeta-object-server' 2>/dev/null); do
		[ "$pid" = "$$" ] && continue
		cmd=$(ps -o command= -p "$pid" 2>/dev/null)
		case "$cmd" in
		*"$REPO_ROOT"* | *"./zeta-object-server"*) ;;
		*) continue ;;
		esac
		ppid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')
		case "$ppid" in
		1) ;; # orphaned - eligible
		*) continue ;; # still owned by a live shell
		esac
		age=$(e2e_proc_age "$pid")
		[ -n "$age" ] || continue
		[ "$age" -ge "$age_cutoff" ] || continue
		kill -TERM "$pid" 2>/dev/null || true
	done
}

# e2e_reap_pid <pid> [label] stops one process this suite started, TERM then
# KILL. Shared by run-e2e.sh's own cleanup so both use the same patience.
e2e_reap_pid() {
	local pid="$1" _i
	[ -n "$pid" ] || return 0
	kill -0 "$pid" 2>/dev/null || return 0
	kill -TERM "$pid" 2>/dev/null
	for _i in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.5
	done
	kill -9 "$pid" 2>/dev/null || true
}
