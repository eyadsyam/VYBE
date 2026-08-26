/// Auth domain entities (§4.1).
///
/// Pure Dart: no Flutter, no Dio, no Drift. That is not stylistic — it is what
/// lets these rules be tested in milliseconds without a widget tester or a
/// database, and what stops a UI concern from quietly becoming a domain one.
library;

/// The age bands §12.4 hangs capability off.
///
/// `under13` exists in the type even though the server refuses those accounts,
/// because the CLIENT is what decides whether to send the request at all —
/// telling a twelve-year-old why before a round trip is better than a 403.
enum AgeBand {
  under13,
  teen13to15,
  teen16to17,
  adult;

  static AgeBand? fromWire(String? value) => switch (value) {
        'under_13' => AgeBand.under13,
        'teen_13_15' => AgeBand.teen13to15,
        'teen_16_17' => AgeBand.teen16to17,
        'adult' => AgeBand.adult,
        _ => null,
      };

  /// FR-2's predicate, stated once.
  bool get isMinorUnder16 =>
      this == AgeBand.under13 || this == AgeBand.teen13to15;
}

/// What the user is entitled to.
enum EntitlementTier {
  free,
  plus;

  static EntitlementTier fromWire(String? value) =>
      value == 'plus' ? EntitlementTier.plus : EntitlementTier.free;

  /// FR-16's room capacity.
  ///
  /// Duplicated from the server deliberately, and ONLY for display — "4 of 8
  /// seats" has to render before any request is made. The server is still the
  /// authority; a client that disagreed would show the wrong number, not admit
  /// an extra person.
  int get maxParticipants => switch (this) {
        EntitlementTier.free => 4,
        EntitlementTier.plus => 8,
      };
}

/// Which digits to render (§3.6).
///
/// A user preference, not a locale consequence. An Arabic speaker may well
/// prefer Western digits, and deriving this from the locale would take the
/// choice away from them.
enum NumeralSystem {
  western,
  eastern;

  static NumeralSystem fromWire(String? value) =>
      value == 'eastern' ? NumeralSystem.eastern : NumeralSystem.western;
}

/// The signed-in user.
///
/// Note what is absent: no email, no date of birth, no token. The wire shape
/// does not carry them and neither does this — a field that does not exist
/// cannot be logged, cached, or rendered by accident.
class Account {
  const Account({
    required this.id,
    required this.handle,
    required this.displayName,
    required this.locale,
    required this.region,
    required this.numeralSystem,
    required this.ageBand,
    required this.entitlementTier,
    required this.isDiscoverable,
    this.avatarUrl,
  });

  final String id;
  final String handle;
  final String displayName;
  final String locale;
  final String region;
  final NumeralSystem numeralSystem;
  final AgeBand ageBand;
  final EntitlementTier entitlementTier;
  final bool isDiscoverable;
  final String? avatarUrl;

  @override
  bool operator ==(Object other) =>
      other is Account &&
      other.id == id &&
      other.handle == handle &&
      other.displayName == displayName &&
      other.locale == locale &&
      other.region == region &&
      other.numeralSystem == numeralSystem &&
      other.ageBand == ageBand &&
      other.entitlementTier == entitlementTier &&
      other.isDiscoverable == isDiscoverable &&
      other.avatarUrl == avatarUrl;

  @override
  int get hashCode => Object.hash(id, handle, displayName, locale, region,
      numeralSystem, ageBand, entitlementTier, isDiscoverable, avatarUrl);
}

/// The minimum age §12.4 sets.
const minimumAgeYears = 13;

/// The password rules FR-1 sets, checked locally before a round trip.
///
/// Local checking is a courtesy, not the enforcement — the server checks the
/// same rules and additionally consults a breach set the client does not have.
/// The value is telling a user their password is too short in 0ms rather than
/// after a request, and NOT telling them it is fine when the server will
/// disagree.
class PasswordRules {
  const PasswordRules._();

  static const minLength = 12;
  static const maxLength = 256;

  /// The local failure, or null when the password passes what we can check.
  static PasswordProblem? check(String password) {
    if (password.length < minLength) return PasswordProblem.tooShort;
    if (password.length > maxLength) return PasswordProblem.tooLong;
    return null;
  }
}

/// Why a password was refused.
///
/// `breached` is never produced locally — only the server can know it — but it
/// is in the enum so the UI has one type to render from, whichever side found
/// the problem.
enum PasswordProblem { tooShort, tooLong, breached }

/// Handle rules FR-1 sets.
///
/// ASCII-only, matching the server, and worth restating why here: a handle
/// appears in URLs and is read aloud, so mixed scripts open homograph
/// impersonation — Cyrillic `а` is indistinguishable from Latin `a` in almost
/// every font. `displayName` is unrestricted Unicode and is the field that
/// actually carries somebody's name, including in Arabic.
class HandleRules {
  const HandleRules._();

  static const minLength = 3;
  static const maxLength = 30;

  static final _pattern = RegExp(r'^[a-z0-9]([a-z0-9._]*[a-z0-9])?$');

  /// Whether a handle is acceptable, after normalisation.
  static bool isValid(String raw) {
    final handle = normalise(raw);
    if (handle.length < minLength || handle.length > maxLength) return false;
    if (!_pattern.hasMatch(handle)) return false;
    // No doubled separators: "a..b" reads as one separator to a human and is a
    // distinct handle to a machine.
    return !handle.contains('..') &&
        !handle.contains('__') &&
        !handle.contains('._') &&
        !handle.contains('_.');
  }

  /// Lowercases and trims, matching the server's normalisation exactly.
  ///
  /// The two must agree or a handle that looks valid here is rejected there.
  static String normalise(String raw) => raw.trim().toLowerCase();
}

/// Derives the age band from a date of birth, matching the server.
///
/// The boundary is "has the birthday occurred yet this year", which a naive
/// year subtraction gets wrong for most of the year — and an off-by-one here
/// puts a 15-year-old in public rooms.
AgeBand deriveAgeBand(DateTime dateOfBirth, DateTime now) {
  var years = now.year - dateOfBirth.year;
  final hadBirthday = now.month > dateOfBirth.month ||
      (now.month == dateOfBirth.month && now.day >= dateOfBirth.day);
  if (!hadBirthday) years -= 1;

  if (years < 13) return AgeBand.under13;
  if (years < 16) return AgeBand.teen13to15;
  if (years < 18) return AgeBand.teen16to17;
  return AgeBand.adult;
}
