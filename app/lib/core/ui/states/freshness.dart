/// Freshness indication (FR-60, AC-32, §11.2).
///
/// > **AC-32** Given cached content older than its TTL, When the screen
/// > renders, Then a visible age indicator is shown ("updated 2h ago") and the
/// > data is not presented as live.
///
/// The requirement is really about honesty. An offline-tolerant app that shows
/// yesterday's room list identically to a live one has not degraded gracefully;
/// it has lied quietly, and the user finds out by acting on stale data. The
/// indicator costs a line of text and removes the whole class of problem.
library;

import 'package:flutter/material.dart';

import '../../../l10n/generated/app_localizations.dart';
import '../tokens.dart';

/// How fresh some rendered data is (§11.2's tiers).
enum Freshness {
  /// Within its TTL. Nothing is shown — a badge on live data is noise.
  live,

  /// Past its TTL but usable, and a refresh is either running or possible.
  stale,

  /// Past its TTL with no way to refresh, because the device is offline.
  offlineStale,
}

/// Formats an age using the `ago*` .arb keys.
///
/// Rounds **down** deliberately. Telling a user their data is younger than it
/// is defeats the point; "2 hours ago" for something 2h59m old is a smaller
/// lie than "3 hours ago" would be a false alarm, and the direction of the
/// error is the part that matters.
String formatAge(Duration age, L10n l10n) {
  if (age.inMinutes < 1) return l10n.agoJustNow;
  if (age.inHours < 1) return l10n.agoMinutes(age.inMinutes);
  if (age.inDays < 1) return l10n.agoHours(age.inHours);
  return l10n.agoDays(age.inDays);
}

/// A single line stating how old the content is.
///
/// Renders nothing when [freshness] is [Freshness.live], so a screen can place
/// it unconditionally and let the state decide — which is what makes it
/// realistic for every data-backed screen to satisfy FR-60 rather than only
/// the ones somebody remembered.
class FreshnessBanner extends StatelessWidget {
  const FreshnessBanner({
    super.key,
    required this.freshness,
    required this.age,
  });

  final Freshness freshness;

  /// How long ago the data was fetched.
  final Duration age;

  @override
  Widget build(BuildContext context) {
    if (freshness == Freshness.live) return const SizedBox.shrink();

    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    final text = freshness == Freshness.offlineStale
        ? l10n.freshnessOfflineStale
        : l10n.freshnessUpdatedAgo(formatAge(age, l10n));

    return Semantics(
      // §3.5: never convey state by colour alone. A muted background says
      // "stale" to a sighted user and nothing at all to anyone else, so the
      // meaning is in the text and the text is announced.
      liveRegion: true,
      child: Container(
        width: double.infinity,
        padding: const EdgeInsetsDirectional.symmetric(
          horizontal: Space.lg,
          vertical: Space.sm,
        ),
        color: theme.colorScheme.surfaceContainerHighest,
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(
              freshness == Freshness.offlineStale
                  ? Icons.cloud_off_outlined
                  : Icons.schedule_outlined,
              size: TypeScale.body,
              color: theme.colorScheme.onSurfaceVariant,
            ),
            const SizedBox(width: Space.sm),
            Expanded(
              child: Text(
                text,
                style: theme.textTheme.bodySmall?.copyWith(
                  fontSize: TypeScale.caption,
                  color: theme.colorScheme.onSurfaceVariant,
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}
