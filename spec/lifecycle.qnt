// The instance lifecycle protocol of cmd/sprout for a single instance id:
// incarnations of the instance directory, the daemon.lock claim, the `up`
// attempt token, and the `delete` incarnation marker.
//
// Each action is one call into the Go code that runs entirely under the
// lifecycle lock (publishAttempt, prepareUpBoot, prepareStartBoot, deleteOne), or one step taken with no lock at all
// (newDeleteTarget, a daemon exiting). The lifecycle lock is therefore not a
// variable: it is what licenses each locked call to be a single atomic step.
// Interleavings inside a locked call are only with unlocked steps, and those
// cannot change what the call observes except through daemon.lock, whose
// release before or after the call both appear as separate traces.
//
// cmd/sprout/lifecycle_model_test.go replays traces of this module against
// the real functions; every `lastStep.act` names the outcome the Go call must
// reproduce.
module lifecycle {
  type Proc = str

  pure val PROCS: Set[Proc] = Set("p1", "p2", "p3")
  // Bounds the incarnations a trace can create, so dlock stays a total map.
  pure val MAX_INO: int = 4
  pure val NONE: str = ""

  // Inode of the instance directory currently at the pathname; 0 is absent.
  var dirIno: int
  var nextIno: int
  // daemon.lock holder per incarnation. A removed incarnation keeps its entry:
  // a held fd still locks the unlinked inode.
  var dlock: int -> str
  // Whether an `up` committed instance.json into each incarnation.
  var recorded: int -> bool
  var pc: Proc -> str
  // Incarnation whose daemon.lock each process holds; 0 is none.
  var claimed: Proc -> int
  // Incarnation each process's `up` attempt token was published into.
  var token: Proc -> int
  // Incarnation each process's `delete` confirmation was asked about.
  var marker: Proc -> int
  // Incarnation each daemon was prepared for: the token's for `up`, the claimed
  // one for `start`.
  var bootedFor: Proc -> int
  var lastStep: { proc: str, act: str, ino: int, confirmed: int }

  action init = all {
    dirIno' = 0,
    nextIno' = 1,
    dlock' = 1.to(MAX_INO).mapBy(_ => NONE),
    recorded' = 1.to(MAX_INO).mapBy(_ => false),
    pc' = PROCS.mapBy(_ => "idle"),
    claimed' = PROCS.mapBy(_ => 0),
    token' = PROCS.mapBy(_ => 0),
    marker' = PROCS.mapBy(_ => 0),
    bootedFor' = PROCS.mapBy(_ => 0),
    lastStep' = { proc: NONE, act: "init", ino: 0, confirmed: 0 },
  }

  def lockFree(ino: int): bool = ino != 0 and dlock.get(ino) == NONE

  // publishAttempt: create the directory if absent, then publish a token in it.
  action upPublish(p: Proc): bool = all {
    pc.get(p) == "idle",
    dirIno != 0 or nextIno <= MAX_INO,
    val ino = if (dirIno == 0) nextIno else dirIno
    all {
      dirIno' = ino,
      nextIno' = if (dirIno == 0) nextIno + 1 else nextIno,
      token' = token.set(p, ino),
      pc' = pc.set(p, "up.built"),
      lastStep' = { proc: p, act: if (dirIno == 0) "up.publish.created" else "up.publish.existing", ino: ino, confirmed: 0 },
    },
    dlock' = dlock, claimed' = claimed, marker' = marker, bootedFor' = bootedFor, recorded' = recorded,
  }

  // prepareUpBoot, after the build ran unlocked: verify the token, claim
  // daemon.lock, verify again, commit the record, retire the token.
  action upBoot(p: Proc): bool = all {
    pc.get(p) == "up.built",
    val stale = token.get(p) != dirIno
    val held = not(stale) and not(lockFree(dirIno))
    all {
      dlock' = if (stale or held) dlock else dlock.set(dirIno, p),
      claimed' = if (stale or held) claimed else claimed.set(p, dirIno),
      bootedFor' = if (stale or held) bootedFor else bootedFor.set(p, token.get(p)),
      recorded' = if (stale or held) recorded else recorded.set(dirIno, true),
      pc' = pc.set(p, if (stale or held) "idle" else "daemon"),
      lastStep' = {
        proc: p,
        act: if (stale) "up.boot.stale" else if (held) "up.boot.held" else "up.boot.ok",
        ino: dirIno,
        confirmed: 0,
      },
    },
    token' = token.set(p, 0),
    dirIno' = dirIno, nextIno' = nextIno, marker' = marker,
  }

  // prepareStartBoot: claim, then boot the bundle already on record.
  action start(p: Proc): bool = all {
    pc.get(p) == "idle",
    val absent = dirIno == 0
    val held = not(absent) and not(lockFree(dirIno))
    val norecord = not(absent or held) and not(recorded.get(dirIno))
    val fails = absent or held or norecord
    all {
      dlock' = if (fails) dlock else dlock.set(dirIno, p),
      claimed' = if (fails) claimed else claimed.set(p, dirIno),
      bootedFor' = if (fails) bootedFor else bootedFor.set(p, dirIno),
      pc' = pc.set(p, if (fails) "idle" else "daemon"),
      lastStep' = {
        proc: p,
        act: if (absent) "start.notfound" else if (held) "start.held" else if (norecord) "start.norecord" else "start.ok",
        ino: dirIno,
        confirmed: 0,
      },
    },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, marker' = marker, recorded' = recorded,
  }

  // The kernel drops the flock however the daemon ends; it takes no lock to exit.
  action daemonExit(p: Proc): bool = all {
    pc.get(p) == "daemon",
    dlock' = dlock.set(claimed.get(p), NONE),
    claimed' = claimed.set(p, 0),
    bootedFor' = bootedFor.set(p, 0),
    pc' = pc.set(p, "idle"),
    lastStep' = { proc: p, act: "daemon.exit", ino: claimed.get(p), confirmed: 0 },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, marker' = marker, recorded' = recorded,
  }

  // newDeleteTarget: the unlocked selection the user then confirms.
  action deleteSelect(p: Proc): bool = all {
    pc.get(p) == "idle",
    marker' = if (dirIno == 0) marker else marker.set(p, dirIno),
    pc' = if (dirIno == 0) pc else pc.set(p, "delete.confirmed"),
    lastStep' = {
      proc: p,
      act: if (dirIno == 0) "delete.select.absent" else "delete.select",
      ino: dirIno,
      confirmed: 0,
    },
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token, bootedFor' = bootedFor,
    recorded' = recorded,
  }

  // deleteOne, with no daemon serving control so the stop branch is not taken.
  action deleteRun(p: Proc): bool = all {
    pc.get(p) == "delete.confirmed",
    val changed = marker.get(p) != dirIno
    val held = not(changed) and not(lockFree(dirIno))
    all {
      dirIno' = if (changed or held) dirIno else 0,
      lastStep' = {
        proc: p,
        act: if (changed) "delete.changed" else if (held) "delete.held" else "delete.removed",
        ino: dirIno,
        confirmed: marker.get(p),
      },
    },
    marker' = marker.set(p, 0),
    pc' = pc.set(p, "idle"),
    nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token, bootedFor' = bootedFor,
    recorded' = recorded,
  }

  action step = {
    nondet p = PROCS.oneOf()
    any {
      upPublish(p),
      upBoot(p),
      start(p),
      daemonExit(p),
      deleteSelect(p),
      deleteRun(p),
    }
  }

  // A running daemon always holds the lock of the directory at the pathname:
  // no delete removed it from under the daemon, and no daemon claimed a husk.
  val daemonOnLiveIncarnation: bool =
    PROCS.forall(p => pc.get(p) == "daemon" implies claimed.get(p) == dirIno)

  // A stale `up` whose instance was deleted mid-build never boots the
  // replacement with its build.
  val daemonBootedForItsIncarnation: bool =
    PROCS.forall(p => pc.get(p) == "daemon" implies bootedFor.get(p) == claimed.get(p))

  val singleDaemon: bool =
    tuples(PROCS, PROCS).forall(((p, q)) =>
      (p != q and pc.get(p) == "daemon" and pc.get(q) == "daemon") implies claimed.get(p) != claimed.get(q))

  // A delete only ever removes the incarnation the user confirmed.
  val deleteHonorsConfirmation: bool =
    lastStep.act == "delete.removed" implies lastStep.ino == lastStep.confirmed

  val safety: bool = all {
    daemonOnLiveIncarnation,
    daemonBootedForItsIncarnation,
    singleDaemon,
    deleteHonorsConfirmation,
  }

  // Witnesses that the simulator reaches the races the invariants are about.
  val staleUpRejected: bool = lastStep.act == "up.boot.stale"
  val swappedDeleteRejected: bool = lastStep.act == "delete.changed"
  val recreated: bool = nextIno > 2
}
