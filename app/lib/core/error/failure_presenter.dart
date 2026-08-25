/// Turns a [Failure] into localised, user-facing text (§3.2, §4.4, FR-61).
///
/// `failure.dart` states the rule this file exists to keep: a `Failure` carries
/// a *cause*, never a sentence. This is the single place a cause becomes words,
/// and therefore the single place those words get translated. Scatter that and
/// Arabic coverage rots one `Text('Something went wrong')` at a time — which
/// §3.6 calls out precisely because it is discovered three weeks before launch.
///
/// The presenter is a pure function of (failure, l10n). It returns data, not
/// widgets, so it is testable in a plain `flutter test` without pumping a
/// widget tree, and so the same presentation can drive a full-screen state, an
/// inline banner, or a snackbar.
library;

import '../../l10n/generated/app_localizations.dart';
import 'failure.dart';

/// What the user can do about a failure.
///
/// §3.2 requires an error state to say what happened, what to do, and offer a
/// support path when it is terminal. Modelling the action as an enum means a
/// new failure variant cannot ship with a dead-end screen: the switch below
/// will not compile until somebody decides what the user's next move is.
enum FailureAction {
  /// Re-run the same request.
  retry,

  /// Go to sign-in. Only offered when signing in would actually help.
  signIn,

  /// Leave for somewhere that works.
  goHome,

  /// Terminal, and the user needs a human.
  contactSupport,

  /// Nothing to offer — the state resolves itself (a rate limit elapsing) or
  /// the caller supplies its own action.
  none,
}

/// A failure, ready to render.
class FailurePresentation {
  const FailurePresentation({
    required this.title,
    required this.body,
    required this.action,
    required this.isRetryable,
    this.traceId,
  });

  final String title;
  final String body;
  final FailureAction action;

  /// Whether re-running the request could plausibly succeed. Drives whether a
  /// retry affordance is shown at all, per §3.2's retryable/terminal split.
  final bool isRetryable;

  /// Shown only on terminal errors, so a user reporting a problem can quote it
  /// and it can be found in the server logs (§14.2, FR-58).
  final String? traceId;
}

/// Maps failures to presentations.
abstract final class FailurePresenter {
  /// The exhaustive switch. Adding a `Failure` variant breaks this at compile
  /// time, which is the whole reason the hierarchy is sealed.
  static FailurePresentation present(Failure failure, L10n l10n) {
    switch (failure) {
      case NetworkFailure(:final isOffline):
        return FailurePresentation(
          // Offline is not an error the user caused, and §3.2 gives it its own
          // state rather than folding it into a generic failure.
          title: isOffline ? l10n.errorOffline : l10n.errorRetryableTitle,
          body: isOffline ? l10n.errorOfflineBody : l10n.errorNetworkBody,
          action: FailureAction.retry,
          isRetryable: true,
        );

      case ServerFailure(:final status, :final traceId):
        // 5xx is ours and is worth retrying; a 4xx that reached here is not
        // one the client knows how to fix, so it is terminal and gets a
        // support path and a trace id.
        final ours = status >= 500;
        return FailurePresentation(
          title: ours ? l10n.errorRetryableTitle : l10n.errorTerminalTitle,
          body: ours ? l10n.errorServerBody : l10n.errorUnexpectedBody,
          action: ours ? FailureAction.retry : FailureAction.contactSupport,
          isRetryable: ours,
          traceId: traceId,
        );

      case AuthFailure(:final reason):
        return _auth(reason, l10n);

      case ValidationFailure():
        // The fields carry their own messages at the form level; this is the
        // summary that sits above them.
        return FailurePresentation(
          title: l10n.errorRetryableTitle,
          body: l10n.errorValidationBody,
          action: FailureAction.none,
          isRetryable: true,
        );

      case RateLimitFailure(:final retryAfter):
        // §3.2: show the time until retry, not "try again later". The real
        // number comes from the server, so it is the real number.
        final seconds = retryAfter.inSeconds;
        return FailurePresentation(
          title: l10n.errorRateLimitedTitle,
          body: seconds <= 0
              ? l10n.errorRateLimitedReady
              : l10n.errorRateLimited(seconds),
          action: seconds <= 0 ? FailureAction.retry : FailureAction.none,
          isRetryable: true,
        );

      case ConflictFailure(:final kind):
        return _conflict(kind, l10n);

      case RequiresConnectionFailure():
        // §11.3 / AC-33: say plainly that this cannot be saved for later. The
        // forbidden alternative is optimistic success followed by a silent drop.
        return FailurePresentation(
          title: l10n.errorOffline,
          body: l10n.errorRequiresConnection,
          action: FailureAction.none,
          isRetryable: true,
        );

      case UnexpectedFailure():
        return FailurePresentation(
          title: l10n.errorTerminalTitle,
          body: l10n.errorUnexpectedBody,
          action: FailureAction.contactSupport,
          isRetryable: false,
        );
    }
  }

  static FailurePresentation _auth(AuthReason reason, L10n l10n) {
    switch (reason) {
      case AuthReason.tokenExpired:
        // Should never reach a screen: the interceptor refreshes and replays
        // (EC-7). If it does, treat it as retryable rather than signing the
        // user out over a transient.
        return FailurePresentation(
          title: l10n.errorRetryableTitle,
          body: l10n.errorNetworkBody,
          action: FailureAction.retry,
          isRetryable: true,
        );

      case AuthReason.sessionRevoked:
        // EC-8 / ADR-011. The user is told WHY, because a silent bounce to
        // sign-in after reuse detection is indistinguishable from a bug and
        // teaches them to distrust the app.
        return FailurePresentation(
          title: l10n.authRequiredTitle,
          body: l10n.errorSessionRevoked,
          action: FailureAction.signIn,
          isRetryable: false,
        );

      case AuthReason.invalidCredentials:
        return FailurePresentation(
          title: l10n.authRequiredTitle,
          body: l10n.errorValidationBody,
          action: FailureAction.none,
          isRetryable: true,
        );

      case AuthReason.notAuthenticated:
        return FailurePresentation(
          title: l10n.authRequiredTitle,
          body: l10n.authRequiredBody,
          action: FailureAction.signIn,
          isRetryable: false,
        );

      case AuthReason.forbidden:
        // Distinct from notAuthenticated on purpose: offering "sign in" to
        // somebody already signed in is nonsense and reads as a broken app.
        return FailurePresentation(
          title: l10n.authForbiddenTitle,
          body: l10n.authForbiddenBody,
          action: FailureAction.goHome,
          isRetryable: false,
        );
    }
  }

  static FailurePresentation _conflict(ConflictKind kind, L10n l10n) {
    switch (kind) {
      case ConflictKind.roomEnded:
        // §3.2 and EC-17 both insist this is a dedicated screen, not a generic
        // error. Tapping an invite to a finished room is a normal thing to do.
        return FailurePresentation(
          title: l10n.roomEndedTitle,
          body: l10n.roomEndedBody,
          action: FailureAction.goHome,
          isRetryable: false,
        );

      case ConflictKind.roomFull:
        return FailurePresentation(
          title: l10n.roomFullTitle,
          body: l10n.roomFullBody,
          action: FailureAction.goHome,
          isRetryable: false,
        );

      case ConflictKind.staleWrite:
        return FailurePresentation(
          title: l10n.errorRetryableTitle,
          body: l10n.errorConflictStaleWrite,
          action: FailureAction.retry,
          isRetryable: true,
        );

      case ConflictKind.invalidState:
        return FailurePresentation(
          title: l10n.errorTerminalTitle,
          body: l10n.errorConflictInvalidState,
          action: FailureAction.goHome,
          isRetryable: false,
        );

      case ConflictKind.duplicate:
        // An idempotent replay (§5.2). The action already succeeded, so
        // presenting failure would be a lie; callers normally treat this as
        // success and never render it.
        return FailurePresentation(
          title: l10n.errorRetryableTitle,
          body: l10n.errorUnexpectedBody,
          action: FailureAction.none,
          isRetryable: false,
        );
    }
  }
}
