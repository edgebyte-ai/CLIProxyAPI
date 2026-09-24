# Codex quota branch boundaries

The quota branch is used with the `codex-quota-scheduler` plugin. The plugin is
the Codex account policy layer; CPA remains the execution and retry layer.

| Responsibility | Owner | Reason to keep it here |
| --- | --- | --- |
| Codex candidate ranking, quota eligibility, CPA-priority fallthrough, reset-aware ordering | Scheduler plugin | The plugin owns quota observations, account annotations, reset policy, and queue state. |
| Codex sessionless binding while the plugin is active | Scheduler plugin | The plugin composes caller scope, provider pool, and model into its own bounded binding. |
| Generic sessionless fill-first binding | CPA selector | This applies to non-Codex providers and remains the fallback when the plugin is disabled or does not handle a request. |
| Complete-response buffering and safe replay | CPA Codex executors | Buffering surrounds the upstream stream and must decide whether output has committed before credential retry. |
| Temporary credential-selection retry and cooldown deadlines | CPA auth conductor | The conductor owns retry rounds, attempted credentials, request cancellation, and provider execution errors. |
| Account-pinned Codex cooldown probe and cooldown-state cleanup | CPA auth conductor | The probe updates CPA's internal model and credential cooldown state; a quota plugin reset probe cannot safely mutate that state. |
| Quota refresh, reset probe state, and management queue | Scheduler plugin | These are plugin-owned observations and UI state. |

The scheduler callback runs before CPA's built-in selector. When the plugin
returns an `AuthID`, CPA validates that the ID is one of the current eligible
candidates and executes it directly. The built-in selector does not select a
second account for that request. If the plugin is disabled, unavailable, or
does not handle another provider, CPA's normal selector remains available.

The branch intentionally retains the generic default-session implementation in
CPA. Removing it would change non-Codex providers and the no-plugin fallback;
it does not duplicate the plugin's Codex binding while the plugin handles that
request.

The branch also retains both probe families. The plugin reset probe detects
lazy quota-window resets from plugin-owned observations. The CPA cooldown probe
is account-pinned and clears CPA model/credential cooldown state after a direct
probe. They have different state owners and are not interchangeable.

The branch-specific changes are therefore limited to the features absent from
main: complete-response buffering, Codex cooldown probing, temporary selection
retry handling, and the generic default-session enhancement. The removed
affinity capability experiment and local release helper are not runtime
features.
