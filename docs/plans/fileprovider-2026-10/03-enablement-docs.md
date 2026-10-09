# Leaf 03 - enablement + distribution documentation

## Goal

Document the manual enablement path for the FileProvider extension built
in leaf 02, and the explicit distribution gate.

## Requirements

1. README section (zeta-cache/README.md): FileProvider setup - build the
   extension, register with `pluginkit -a <appex>` (or the documented
   Xcode-hosted path), select the FileProvider domain in Finder/Storage
   settings, verify with `fileproviderctl`. Mechanism-before-name, plain
   sentences, hyphens not em-dashes.
2. Signing/notarization: documented as REQUIRED for distribution to any
   machine other than the build machine (codesign identity, notarytool
   invocation outline) - explicit future-work gate, not automated.
3. The LaunchAgent plist from leaf 01 cross-referenced (the daemon must
   run at login for Finder to see a live namespace).
4. Known limitations table: no app-container Replicated extension
   (classic API), no shared-lock semantics (server supports exclusive
   only), eviction is daemon-owned (Finder "Remove Download" maps to
   IPC dehydrate), credential-free extension (the daemon holds auth).
5. zeta-object#14 comment + close-or-keep decision: post the shipped
   state as a comment; keep the issue OPEN only if remaining gaps
   justify it (recommendation: close citing this tree).

## Acceptance

1. Every documented command verified by hand on this machine where
   possible (build, pluginkit registration, fileproviderctl status);
   anything not verifiable locally is marked manual.
2. README renders the full setup in <= 40 lines.
