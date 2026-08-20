import Foundation
import Testing

@testable import ModelRouterTray

// 悬浮岛/托盘装饰动画从 withAnimation(.repeatForever) 隐式驱动改为
// TimelineView 12fps 显式驱动后，相位/位移全靠这里的纯函数。它们决定
// 动画走多快、停在哪个档位，所以边界和周期性必须有测试兜底。
@Suite("Low-frequency island animation")
struct LowFrequencyAnimationTests {
  @Test("呼吸相位始终落在 0...1，且按周期往返", arguments: [1.35, 1.44, 1.8, 3.2])
  func breathPhaseStaysInRange(duration: Double) {
    var step = 0.0
    while step < duration {
      let phase = IslandAnimation.breathPhase(
        at: Date(timeIntervalSinceReferenceDate: step),
        duration: duration
      )
      #expect(phase >= 0 && phase <= 1)
      step += duration / 32
    }

    // 周期性：相隔一个周期的相位一致；半周期处到达峰值。
    let t = 123.456
    let a = IslandAnimation.breathPhase(at: Date(timeIntervalSinceReferenceDate: t), duration: duration)
    let b = IslandAnimation.breathPhase(
      at: Date(timeIntervalSinceReferenceDate: t + duration),
      duration: duration
    )
    #expect(abs(a - b) < 1e-9)
    let mid = IslandAnimation.breathPhase(
      at: Date(timeIntervalSinceReferenceDate: duration / 2),
      duration: duration
    )
    #expect(abs(mid - 1) < 1e-9)
  }

  @Test("呼吸相位起点为 0，避免切态时跳变")
  func breathPhaseStartsAtZero() {
    #expect(
      IslandAnimation.breathPhase(at: Date(timeIntervalSinceReferenceDate: 0), duration: 1.35) < 1e-9
    )
  }

  @Test("线性循环进度落在 0..<1 且按周期回绕")
  func loopProgressWraps() {
    let duration = 3.2
    #expect(IslandAnimation.loopProgress(at: Date(timeIntervalSinceReferenceDate: 0), duration: duration) == 0)
    let p = IslandAnimation.loopProgress(
      at: Date(timeIntervalSinceReferenceDate: 100.9),
      duration: duration
    )
    #expect(p >= 0 && p < 1)
    let q = IslandAnimation.loopProgress(
      at: Date(timeIntervalSinceReferenceDate: 100.9 + duration),
      duration: duration
    )
    #expect(abs(p - q) < 1e-9)
  }

  @Test("easeInOut 近似端点收敛、中点对称")
  func easeInOutEndpoints() {
    #expect(abs(IslandAnimation.easeInOut(0)) < 1e-9)
    #expect(abs(IslandAnimation.easeInOut(1) - 1) < 1e-9)
    #expect(abs(IslandAnimation.easeInOut(0.5) - 0.5) < 1e-9)
    #expect(IslandAnimation.easeInOut(-3) == 0)
    #expect(IslandAnimation.easeInOut(9) == 1)
  }

  @Test("跑马灯位移被夹在 -overflow...0，且按循环回绕")
  func marqueeOffsetStaysWithinBounds() {
    let overflow = 120.0
    let travel = 2.8
    let pause = 0.7
    let cycle = 2 * travel + 2 * pause
    var step = 0.0
    while step < cycle {
      let offset = IslandAnimation.marqueeOffset(
        elapsed: step + 1000 * cycle,
        overflow: overflow,
        travelDuration: travel,
        pause: pause
      )
      #expect(offset <= 0 && offset >= -overflow)
      step += cycle / 64
    }
  }

  @Test("跑马灯关键节点：起点 0、行进末端与停顿区 -overflow、回到 0")
  func marqueeOffsetKeyframes() {
    let overflow = 90.0
    let travel = 3.0
    let pause = 0.7
    #expect(
      IslandAnimation.marqueeOffset(elapsed: 0, overflow: overflow, travelDuration: travel, pause: pause) == 0
    )
    #expect(
      abs(
        IslandAnimation.marqueeOffset(elapsed: travel, overflow: overflow, travelDuration: travel, pause: pause)
          + overflow
      ) < 1e-9
    )
    #expect(
      abs(
        IslandAnimation.marqueeOffset(
          elapsed: travel + pause / 2,
          overflow: overflow,
          travelDuration: travel,
          pause: pause
        ) + overflow
      ) < 1e-9
    )
    #expect(
      abs(
        IslandAnimation.marqueeOffset(
          elapsed: 2 * travel + 1.5 * pause,
          overflow: overflow,
          travelDuration: travel,
          pause: pause
        )
      ) < 1e-9
    )
  }

  @Test("溢出不明显时跑马灯完全不动")
  func marqueeSkipsTinyOverflow() {
    #expect(
      IslandAnimation.marqueeOffset(elapsed: 42, overflow: 1.5, travelDuration: 2.8, pause: 0.7) == 0
    )
  }

  @Test("呼吸点仅在 generating/starting 且未开启减弱动态时运行")
  func beaconGating() {
    #expect(!IslandAnimation.beaconBreathing(state: .idle, reduceMotion: false))
    #expect(IslandAnimation.beaconBreathing(state: .generating, reduceMotion: false))
    #expect(IslandAnimation.beaconBreathing(state: .starting, reduceMotion: false))
    #expect(!IslandAnimation.beaconBreathing(state: .error, reduceMotion: false))
    #expect(!IslandAnimation.beaconBreathing(state: .generating, reduceMotion: true))
  }

  @Test("岛屿光效 idle 静态化，仅 starting/generating 驱动时间线")
  func glowGating() {
    #expect(!IslandAnimation.glowAnimating(state: .idle, reduceMotion: false))
    #expect(IslandAnimation.glowAnimating(state: .starting, reduceMotion: false))
    #expect(IslandAnimation.glowAnimating(state: .generating, reduceMotion: false))
    #expect(!IslandAnimation.glowAnimating(state: .error, reduceMotion: false))
    #expect(!IslandAnimation.glowAnimating(state: .generating, reduceMotion: true))
  }
}
