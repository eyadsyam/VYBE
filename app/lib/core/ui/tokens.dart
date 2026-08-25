/// Design tokens — the single source of visual truth.
///
/// §0.6 lists "40 screens, none of which survive a bad network" and template
/// sameness as tells of a generated project. Tokens do not prevent that on
/// their own, but they make the alternative — a magic number typed into each
/// widget — visibly wrong at review time.
///
/// Constraints these encode, from the master prompt:
///
/// * §3.5 — minimum touch target 48dp; contrast >= 4.5:1 body, >= 3:1 large.
/// * §3.4 — motion budget: nothing blocking over 400ms.
/// * §3.6 — spacing is directional; there is no `left` or `right` here at all.
library;

import 'package:flutter/widgets.dart';

/// Spacing scale. A 4dp base, because 8dp alone is too coarse for dense
/// surfaces like the chat list and forces half-steps to be invented locally.
abstract final class Space {
  static const double xxs = 2;
  static const double xs = 4;
  static const double sm = 8;
  static const double md = 12;
  static const double lg = 16;
  static const double xl = 24;
  static const double xxl = 32;
  static const double xxxl = 48;

  /// §3.5: minimum touch target. Every interactive element is at least this
  /// on both axes — including icon-only buttons, which are the usual offender.
  static const double minTouchTarget = 48;

  /// Default screen inset. Directional, per §3.6 — never `EdgeInsets.only(left:)`.
  static const EdgeInsetsDirectional screen =
      EdgeInsetsDirectional.symmetric(horizontal: lg, vertical: md);
}

/// Corner radii.
abstract final class Radii {
  static const Radius xs = Radius.circular(4);
  static const Radius sm = Radius.circular(8);
  static const Radius md = Radius.circular(12);
  static const Radius lg = Radius.circular(20);
  static const Radius pill = Radius.circular(999);

  static const BorderRadius cardBorder = BorderRadius.all(md);
  static const BorderRadius sheetBorder =
      BorderRadius.vertical(top: lg);
}

/// Motion budget (§3.4).
///
/// The forbidden column of that table is enforced by these being the only
/// durations available: there is deliberately no constant longer than
/// [achievementUnlock], so a 900ms hero transition cannot be written without
/// adding a token and defending it in review.
abstract final class Motion {
  /// Poster to detail hero. §3.4 caps this at 300ms.
  static const Duration hero = Duration(milliseconds: 280);

  /// Standard control feedback.
  static const Duration fast = Duration(milliseconds: 120);
  static const Duration normal = Duration(milliseconds: 200);

  /// §3.4: no blocking animation over 400ms.
  static const Duration slowest = Duration(milliseconds: 380);

  /// Achievement unlock. §3.4 allows up to 600ms **provided it is skippable**;
  /// the widget that plays this must accept a tap to dismiss.
  static const Duration achievementUnlock = Duration(milliseconds: 560);

  /// Total stagger budget for a list, §3.4. Not per item — the whole run.
  static const Duration listStaggerTotal = Duration(milliseconds: 150);

  /// Resolves a duration against the platform reduced-motion setting.
  ///
  /// §3.4 requires all motion to respect `MediaQuery.disableAnimations`.
  /// Routing every duration through here means honouring it is the default
  /// rather than something each animation has to remember.
  static Duration resolve(BuildContext context, Duration desired) =>
      MediaQuery.maybeDisableAnimationsOf(context) ?? false
          ? Duration.zero
          : desired;
}

/// Reaction burst limits (§3.4): "GPU-cheap, capped at 20 concurrent particles".
abstract final class ReactionBudget {
  static const int maxConcurrentParticles = 20;

  /// §7.6: the client batches reaction taps before sending, so a fast tapper
  /// produces one message rather than thirty.
  static const Duration clientBatchWindow = Duration(milliseconds: 250);
}

/// Elevation, expressed as opacity of an overlay rather than a shadow, so it
/// behaves identically in light and dark themes.
abstract final class Elevations {
  static const double none = 0;
  static const double card = 0.04;
  static const double sheet = 0.08;
  static const double dialog = 0.12;
}

/// Typography scale. Sizes only — families and weights live in the theme, so a
/// brand change touches one file.
///
/// Every size here must remain legible at 200% text scale without truncation
/// (NFR-16), which is why the scale is compact: a 34dp display style becomes
/// 68dp at 200% and will not fit a phone.
abstract final class TypeScale {
  static const double display = 28;
  static const double headline = 22;
  static const double title = 18;
  static const double body = 15;
  static const double label = 13;
  static const double caption = 11;

  /// §3.5: text must scale to 200% without truncation or overlap.
  static const double maxSupportedScale = 2.0;
}

/// Breakpoints. VYBE is mobile-first; [tablet] exists so the room screen can
/// use the extra width for chat beside the timeline rather than below it.
abstract final class Breakpoints {
  static const double compact = 600;
  static const double tablet = 905;

  static bool isCompact(BuildContext context) =>
      MediaQuery.sizeOf(context).width < compact;
}
