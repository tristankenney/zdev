---
description: Show the ranked attention queue — who needs me, in what order
---
Run `zdev triage` (Bash). Present the queue compactly: project, state glyph
meaning, wait age, and the captured wait context. If the queue is empty, say
the fleet is quiet. If anything is DEAD, lead with it. Offer to jump
(`zdev <project>`) or answer cheap waits directly from the context shown.
