import { request } from './client'

export type OnboardingStepStatus = 'not_started' | 'completed' | 'skipped'
export type OnboardingStep = 'preference' | 'portfolio' | 'alert'

export interface OnboardingProgress {
  id: number
  version: number
  run: number
  status: 'in_progress' | 'completed'
  preference_status: OnboardingStepStatus
  preference_at?: string
  portfolio_status: OnboardingStepStatus
  portfolio_at?: string
  alert_status: OnboardingStepStatus
  alert_at?: string
  alert_rule_id?: number
  alert_tested_at?: string
  deferred_until?: string
  completed_at?: string
  created_at: string
  updated_at: string
  should_prompt: boolean
  suggested_step: OnboardingStep | 'complete'
}

export function getOnboardingProgress() {
  return request<OnboardingProgress>({ url: '/onboarding', method: 'get' })
}

export function skipOnboardingStep(step: OnboardingStep) {
  return request<OnboardingProgress>({ url: `/onboarding/steps/${step}/skip`, method: 'post' })
}

export function finishOnboarding() {
  return request<OnboardingProgress>({ url: '/onboarding/finish', method: 'post' })
}

export function restartOnboarding() {
  return request<OnboardingProgress>({ url: '/onboarding/restart', method: 'post' })
}

export function deferOnboarding() {
  return request<OnboardingProgress>({ url: '/onboarding/defer', method: 'post' })
}
