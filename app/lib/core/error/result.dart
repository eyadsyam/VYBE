/// `Result<T>` — the return type of every repository method (§4.4).
///
/// Why a Result rather than exceptions: an exception is invisible in a method
/// signature, so nothing forces a caller to consider the failure path. §3.2
/// requires every data-backed screen to implement an error state, and a type
/// that makes failure unignorable is what turns that from a review checklist
/// item into a compile-time obligation.
library;

import 'failure.dart';

sealed class Result<T> {
  const Result();

  const factory Result.ok(T value) = Ok<T>;
  const factory Result.err(Failure failure) = Err<T>;

  bool get isOk => this is Ok<T>;
  bool get isErr => this is Err<T>;

  /// The value, or null. Prefer pattern matching; this is for the cases where
  /// a null is genuinely the right fallback.
  T? get valueOrNull => switch (this) {
        Ok<T>(:final value) => value,
        Err<T>() => null,
      };

  /// The failure, or null.
  Failure? get failureOrNull => switch (this) {
        Ok<T>() => null,
        Err<T>(:final failure) => failure,
      };

  /// Exhaustive handling. The compiler rejects a caller that forgets a branch.
  R fold<R>({
    required R Function(T value) ok,
    required R Function(Failure failure) err,
  }) =>
      switch (this) {
        Ok<T>(:final value) => ok(value),
        Err<T>(:final failure) => err(failure),
      };

  /// Transforms a success value, leaving a failure untouched.
  ///
  /// This is the mapping seam of §4.1: a repository maps a DTO to a domain
  /// entity here without ever unwrapping the failure path.
  Result<R> map<R>(R Function(T value) transform) => switch (this) {
        Ok<T>(:final value) => Ok<R>(transform(value)),
        Err<T>(:final failure) => Err<R>(failure),
      };

  /// Chains another fallible operation. Short-circuits on the first failure.
  Result<R> flatMap<R>(Result<R> Function(T value) transform) =>
      switch (this) {
        Ok<T>(:final value) => transform(value),
        Err<T>(:final failure) => Err<R>(failure),
      };

  /// Replaces the failure, leaving a success untouched. Used at layer
  /// boundaries to translate a transport failure into a domain one.
  Result<T> mapErr(Failure Function(Failure failure) transform) =>
      switch (this) {
        Ok<T>() => this,
        Err<T>(:final failure) => Err<T>(transform(failure)),
      };

  /// The value, or a fallback. Never throws.
  T getOrElse(T Function(Failure failure) orElse) => switch (this) {
        Ok<T>(:final value) => value,
        Err<T>(:final failure) => orElse(failure),
      };
}

final class Ok<T> extends Result<T> {
  const Ok(this.value);

  final T value;

  @override
  bool operator ==(Object other) =>
      identical(this, other) || (other is Ok<T> && other.value == value);

  @override
  int get hashCode => Object.hash(Ok<T>, value);

  @override
  String toString() => 'Ok($value)';
}

final class Err<T> extends Result<T> {
  const Err(this.failure);

  final Failure failure;

  @override
  bool operator ==(Object other) =>
      identical(this, other) || (other is Err<T> && other.failure == failure);

  @override
  int get hashCode => Object.hash(Err<T>, failure);

  @override
  String toString() => 'Err(${failure.code})';
}

/// Runs [body], converting any thrown object into an `Err`.
///
/// This is the ONE sanctioned place where an exception becomes a Result, and
/// it belongs at the repository boundary. It deliberately does not classify
/// the error: mapping a `DioException` to a `NetworkFailure` requires
/// transport knowledge that only the network layer has, so [onError] is
/// required rather than defaulted. A default here would quietly turn every
/// unclassified error into `UnexpectedFailure` and hide real bugs.
Future<Result<T>> guardAsync<T>(
  Future<T> Function() body, {
  required Failure Function(Object error, StackTrace stackTrace) onError,
}) async {
  try {
    return Ok(await body());
  } catch (error, stackTrace) {
    return Err(onError(error, stackTrace));
  }
}
