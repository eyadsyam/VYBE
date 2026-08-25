/// The §3.2 state widgets, which FR-60 requires every data-backed screen to
/// implement.
///
/// §0.6 names "40 screens, none of which survive a bad network" as the tell of
/// a generated project. These widgets are the counter-measure: the states are
/// built once, properly, so that using them is easier than writing a bare
/// `CircularProgressIndicator` and calling the screen done.
///
/// Every view here is:
///
/// * **Directional** (§3.6) — `EdgeInsetsDirectional`, `start`/`end`. There is
///   no `left` or `right` in this file, so RTL needs no special case.
/// * **Announced** (§3.5) — each carries semantics, because a loading state
///   that is only a spinning shape does not exist for a screen reader.
/// * **Localised** (FR-61) — every string comes from an .arb file.
/// * **Legible at 200% scale** (NFR-16) — content scrolls rather than clipping.
library;

import 'dart:async';

import 'package:flutter/material.dart';

import '../../error/failure.dart';
import '../../error/failure_presenter.dart';
import '../../../l10n/generated/app_localizations.dart';
import '../tokens.dart';

/// Shared scaffold for the centred, message-bearing states.
///
/// It scrolls. That is not incidental: at 200% text scale a title, a body and
/// a button no longer fit a phone, and NFR-16 requires no truncation — so the
/// content must be free to exceed the viewport and be reached.
class _CenteredState extends StatelessWidget {
  const _CenteredState({
    required this.icon,
    required this.title,
    required this.body,
    this.actionLabel,
    this.onAction,
    this.footnote,
  });

  final IconData icon;
  final String title;
  final String body;
  final String? actionLabel;
  final VoidCallback? onAction;
  final String? footnote;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);

    return LayoutBuilder(
      builder: (context, constraints) => SingleChildScrollView(
        padding: Space.screen,
        child: ConstrainedBox(
          constraints: BoxConstraints(minHeight: constraints.maxHeight),
          child: Column(
            mainAxisAlignment: MainAxisAlignment.center,
            crossAxisAlignment: CrossAxisAlignment.center,
            children: [
              Icon(
                icon,
                size: Space.xxxl,
                color: theme.colorScheme.onSurfaceVariant,
                // The icon repeats what the text says, so announcing it too
                // would make a screen reader say everything twice.
                semanticLabel: null,
              ),
              const SizedBox(height: Space.lg),
              Text(
                title,
                textAlign: TextAlign.center,
                style: theme.textTheme.titleLarge?.copyWith(
                  fontSize: TypeScale.title,
                  fontWeight: FontWeight.w600,
                ),
              ),
              const SizedBox(height: Space.sm),
              Text(
                body,
                textAlign: TextAlign.center,
                style: theme.textTheme.bodyMedium?.copyWith(
                  fontSize: TypeScale.body,
                  color: theme.colorScheme.onSurfaceVariant,
                ),
              ),
              if (actionLabel != null && onAction != null) ...[
                const SizedBox(height: Space.xl),
                FilledButton(
                  onPressed: onAction,
                  child: Text(actionLabel!),
                ),
              ],
              if (footnote != null) ...[
                const SizedBox(height: Space.lg),
                // Selectable so a user reporting a problem can copy the trace
                // id rather than transcribe it from a screenshot (§14.2).
                SelectableText(
                  footnote!,
                  textAlign: TextAlign.center,
                  style: theme.textTheme.bodySmall?.copyWith(
                    fontSize: TypeScale.caption,
                    color: theme.colorScheme.onSurfaceVariant,
                  ),
                ),
              ],
            ],
          ),
        ),
      ),
    );
  }
}

/// First load, with nothing to show yet (§3.2).
///
/// A skeleton rather than a spinner: it communicates the shape of what is
/// coming, so the layout does not jump when content arrives.
class LoadingStateView extends StatelessWidget {
  const LoadingStateView({super.key, this.itemCount = 5});

  final int itemCount;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);

    return Semantics(
      // Without this the state is a set of grey rectangles and is invisible to
      // a screen reader (§3.5).
      label: l10n.stateLoading,
      liveRegion: true,
      child: ListView.separated(
        padding: Space.screen,
        itemCount: itemCount,
        separatorBuilder: (_, _) => const SizedBox(height: Space.md),
        itemBuilder: (context, _) => const _SkeletonRow(),
      ),
    );
  }
}

class _SkeletonRow extends StatelessWidget {
  const _SkeletonRow();

  @override
  Widget build(BuildContext context) {
    final base = Theme.of(context).colorScheme.onSurface.withValues(alpha: 0.06);

    return Row(
      children: [
        Container(
          width: Space.xxxl,
          height: Space.xxxl,
          decoration: BoxDecoration(
            color: base,
            borderRadius: Radii.cardBorder,
          ),
        ),
        const SizedBox(width: Space.md),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Container(height: Space.md, color: base),
              const SizedBox(height: Space.sm),
              FractionallySizedBox(
                // Directional: in RTL this shortens from the correct edge
                // without any mirroring code.
                alignment: AlignmentDirectional.centerStart,
                widthFactor: 0.6,
                child: Container(height: Space.md, color: base),
              ),
            ],
          ),
        ),
      ],
    );
  }
}

/// Loaded successfully, and genuinely empty (§3.2).
///
/// Distinct from loading and from error, because "no rooms yet" is a normal,
/// non-failing outcome that deserves an invitation rather than an apology.
class EmptyStateView extends StatelessWidget {
  const EmptyStateView({
    super.key,
    this.title,
    this.body,
    this.actionLabel,
    this.onAction,
  });

  final String? title;
  final String? body;
  final String? actionLabel;
  final VoidCallback? onAction;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    return _CenteredState(
      icon: Icons.inbox_outlined,
      title: title ?? l10n.stateEmptyTitle,
      body: body ?? l10n.stateEmptyBody,
      actionLabel: actionLabel,
      onAction: onAction,
    );
  }
}

/// A failure (§3.2).
///
/// Takes a [Failure], not a string. All the wording and the retry/terminal
/// decision come from [FailurePresenter], so a screen cannot accidentally
/// invent its own English copy — which is what FR-61 forbids.
class ErrorStateView extends StatelessWidget {
  const ErrorStateView({
    super.key,
    required this.failure,
    this.onRetry,
    this.onSignIn,
    this.onGoHome,
    this.onContactSupport,
  });

  final Failure failure;
  final VoidCallback? onRetry;
  final VoidCallback? onSignIn;
  final VoidCallback? onGoHome;
  final VoidCallback? onContactSupport;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final p = FailurePresenter.present(failure, l10n);

    final (label, callback) = switch (p.action) {
      FailureAction.retry => (l10n.errorRetry, onRetry),
      FailureAction.signIn => (l10n.authSignInAction, onSignIn),
      FailureAction.goHome => (l10n.actionGoHome, onGoHome),
      FailureAction.contactSupport => (l10n.errorSupportAction, onContactSupport),
      FailureAction.none => (null, null),
    };

    return _CenteredState(
      icon: p.isRetryable ? Icons.refresh_outlined : Icons.error_outline,
      title: p.title,
      body: p.body,
      actionLabel: callback == null ? null : label,
      onAction: callback,
      // §3.2 asks for a support path on terminal errors; a trace id is what
      // makes that path lead anywhere (FR-58, §14.2).
      footnote: p.traceId == null ? null : l10n.errorTraceId(p.traceId!),
    );
  }
}

/// The device is offline and there is nothing cached (§3.2).
class OfflineStateView extends StatelessWidget {
  const OfflineStateView({super.key, this.onRetry});

  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    return _CenteredState(
      icon: Icons.cloud_off_outlined,
      title: l10n.errorOffline,
      body: l10n.errorOfflineBody,
      actionLabel: onRetry == null ? null : l10n.errorRetry,
      onAction: onRetry,
    );
  }
}

/// Signed out, on a surface that requires an account (§3.2).
///
/// A screen, not a silent redirect. §3.2 forbids bouncing the user to sign-in
/// without saying why — from their side that is indistinguishable from the app
/// losing their place.
class UnauthorisedStateView extends StatelessWidget {
  const UnauthorisedStateView({super.key, this.onSignIn, this.reason});

  final VoidCallback? onSignIn;

  /// Set when the session was revoked, so the copy can explain that rather
  /// than implying the user simply signed out (EC-8, ADR-011).
  final AuthReason? reason;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final revoked = reason == AuthReason.sessionRevoked;

    return _CenteredState(
      icon: Icons.lock_outline,
      title: l10n.authRequiredTitle,
      body: revoked ? l10n.errorSessionRevoked : l10n.authRequiredBody,
      actionLabel: onSignIn == null ? null : l10n.authSignInAction,
      onAction: onSignIn,
    );
  }
}

/// The thing does not exist (§3.2).
class NotFoundStateView extends StatelessWidget {
  const NotFoundStateView({super.key, this.onGoHome});

  final VoidCallback? onGoHome;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    return _CenteredState(
      icon: Icons.search_off_outlined,
      title: l10n.notFoundTitle,
      body: l10n.notFoundBody,
      actionLabel: onGoHome == null ? null : l10n.actionGoHome,
      onAction: onGoHome,
    );
  }
}

/// Rate limited (§3.2, §12.3, AC-23).
///
/// Shows the real remaining time, counted down live. "Try again later" is the
/// version of this screen that tells the user nothing and gets tapped
/// repeatedly, which is exactly what a rate limit is trying to stop.
class RateLimitedStateView extends StatefulWidget {
  const RateLimitedStateView({
    super.key,
    required this.retryAfter,
    this.onRetry,
  });

  final Duration retryAfter;
  final VoidCallback? onRetry;

  @override
  State<RateLimitedStateView> createState() => _RateLimitedStateViewState();
}

class _RateLimitedStateViewState extends State<RateLimitedStateView> {
  late int _remaining;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _remaining = widget.retryAfter.inSeconds;
    _startTicking();
  }

  @override
  void didUpdateWidget(RateLimitedStateView oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.retryAfter != widget.retryAfter) {
      _remaining = widget.retryAfter.inSeconds;
      _startTicking();
    }
  }

  void _startTicking() {
    _timer?.cancel();
    if (_remaining <= 0) return;
    _timer = Timer.periodic(const Duration(seconds: 1), (timer) {
      if (!mounted) return;
      setState(() {
        _remaining--;
        if (_remaining <= 0) timer.cancel();
      });
    });
  }

  @override
  void dispose() {
    // Leaking this timer would keep calling setState after the route is gone.
    _timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final ready = _remaining <= 0;

    return _CenteredState(
      icon: Icons.hourglass_empty_outlined,
      title: l10n.errorRateLimitedTitle,
      body: ready ? l10n.errorRateLimitedReady : l10n.errorRateLimited(_remaining),
      actionLabel: ready && widget.onRetry != null ? l10n.errorRetry : null,
      onAction: ready ? widget.onRetry : null,
    );
  }
}
