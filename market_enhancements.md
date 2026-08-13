# BiteMatch MVP: Market Enhancements Report

> **Date:** 2026-07-14  
> **Status:** DONE  
> **Scope:** Competitor analysis, market gaps, monetization strategies, retention mechanics → Top 5 high-impact, low-effort enhancements for BiteMatch MVP

---

## 1. Competitor Analysis

### 1.1 Hangrily (hangrily.app)

**What it is:** The most mature swipe-based restaurant decision app. Web-first (PWA), also available on iOS and Android.

**Features:**
- Tinder-style swipe cards (Dislike 👎, Like 👍, Love ❤️) with 3 rounds max
- **Couple Mode** — link partner's account once, auto-join sessions, simultaneous swiping, only mutual matches shown
- **Group Mode** — up to 10 participants via 6-digit session code, democratic voting (50%+ approval to advance)
- AI Smart Search — natural language queries ("spicy tacos under $15 within 3 miles")
- Restaurant analytics — decision history, cuisine breakdown, swipe stats, export data
- One-tap directions via map integration
- Dark theme, session timer, no ads

**Monetization:** Free tier (generous) + Premium subscription (pricing not publicly listed; described as "affordable"). No affiliate or B2B revenue streams identified.

**User Acquisition:** SEO-optimized blog/content pages targeting long-tail queries ("where should I eat," "restaurant decision app," "tinder for restaurants"). No account required to start. Claims "zero ads."

**Verdict:** Most feature-complete competitor. Weaknesses: web-first (not native), no loyalty integration, no social network layer, no affiliate/commission revenue, no gamification beyond swiping.

**Sources:** [hangrily.app/features](https://hangrily.app/features), [hangrily.app/tinder-for-restaurants](https://hangrily.app/tinder-for-restaurants)

---

### 1.2 Pickr (alainazemanick.com/portfolio/pickr / Google Play: com.future.proof.apps.decider)

**What it is:** Two distinct products share the "Pickr" name:
1. A portfolio project by designer Alaina Zemanick — a native mobile app for group restaurant selection via Tinder-style swiping (not publicly available as a standalone app).
2. **Pickr — Smart Decision Maker** on Google Play — a general-purpose decision wheel/randomizer app (not restaurant-specific).

**Features (portfolio version):** Swipe left/right on restaurants in group settings, algorithm selects most-selected restaurant, accommodates dietary restrictions and budget.

**Monetization:** Not available as a commercial product.

**Verdict:** Conceptually similar to BiteMatch but never launched as a standalone app. The general "Pickr" decision-maker app on Google Play is not restaurant-focused. **Low competitive threat.**

**Sources:** [alainazemanick.com/portfolio/pickr](https://www.alainazemanick.com/portfolio/pickr), [Google Play: Pickr](https://play.google.com/store/apps/details?id=com.future.proof.apps.decider)

---

### 1.3 DoorDash Zesty (zesty.app) — **SHUTDOWN**

**What it was:** DoorDash's AI-powered social restaurant discovery app. Launched December 2025 in NYC and SF Bay Area beta.

**Features:**
- AI chatbot recommendations ("build your going out vibe")
- Social feed — users post photos/reviews of dining experiences
- Rate restaurants: "Loved this!", "Kinda mid", "Not for me"
- Follow other users ("Zesties"), leaderboard of "Top Zesties"
- Map-based discovery
- No ordering function — purely discovery-first

**Monetization:** No direct monetization identified (DoorDash internal experiment).

**Status:** **Wound down April 2026** — less than 6 months after launch. Core features being folded into the main DoorDash app. [Source: NRN, April 2026](https://www.nrn.com/restaurant-technology/doordash-winds-down-restaurant-discovery-app-zesty)

**Verdict:** DoorDash's failure here is instructive. Zesty proved there's demand for AI-powered discovery but failed because: (a) standalone app with no ordering hook, (b) social layer was too heavy for a decision problem, (c) DoorDash's core app absorbed the tech. **Opportunity for BiteMatch: keep it simple, keep the swipe mechanic, avoid building a social network.**

**Sources:** [NRN: DoorDash winds down Zesty](https://www.nrn.com/restaurant-technology/doordash-winds-down-restaurant-discovery-app-zesty), [Restaurant Business Online: Zesty testing](https://restaurantbusinessonline.com/technology/doordash-testing-restaurant-discovery-app-called-zesty), [Bloomberg: Zesty launch](https://www.bloomberg.com/news/articles/2025-12-16/doordash-launches-zesty-an-ai-powered-app-for-finding-new-restaurants)

---

### 1.4 Other Notable Competitors

| App | Type | Status | Key Differentiator |
|-----|------|--------|-------------------|
| **Munch** | Tinder-for-food group dining | Launched 2021 (NYC), unclear current status | Group swiping with cuisine/location/dietary filters; free to use; restaurants listed at no cost |
| **Grublr** | Swipe-based restaurant picker | Active on TikTok/social media | Swipe through restaurant profiles, menus, reviews; social media buzz |
| **Chicken Tender** | Group restaurant picker | Active on iOS | Bluetooth-based group sync (no invites/links needed); swipe + match |
| **Food Flip** | Tournament-bracket restaurant picker | Active on iOS, IAP | Bracket-style elimination (not swipe), favorite spots, "flip a coin" randomizer |
| **Food Swiper** | Group swipe discovery | Active on iOS | Real-time group sessions, no account required, match history |
| **Hungree** | Group restaurant decision | Active (Canada) | Free group dining decision app |

**Sources:** [AlleyWatch: Munch](https://www.alleywatch.com/2021/02/munch-group-dining-discovery-app-chris-desantis), [Apple App Store: Chicken Tender](https://apps.apple.com/us/app/chicken-tender/id1514175364), [Apple App Store: Food Flip](https://apps.apple.com/us/app/food-flip-decide-where-to-eat/id6740217074), [Apple App Store: Food Swiper](https://apps.apple.com/us/app/food-swiper-find-food/id1581852879), [hungree.ca](https://hungree.ca)

---

## 2. Market Gaps — What Competitors Are Missing

### 2.1 The Decision Fatigue Problem Is Real and Growing

- **Nextatlas data** shows conversations around "decision fatigue in food and beverage" have grown **114%+ year-over-year** ([NextAtlas](https://blog.nextatlas.com/article/exhausted-eater-how-decision-fatigue-is-reshaping-food-and-beverage-with-future-food-institute)).
- **55% of U.S. consumers now trust AI-generated review summaries** for venue discovery, with 45% of 35–44 year-olds using ChatGPT for restaurant discovery ([Reputation/Nielsen survey](https://reputation.com/resources/press/new-reputation-survey-reveals-how-ai-and-economic-pressures-are-rewriting-the-rules-of-dining-out-in-america)).
- **64% of U.S. adults tried a new restaurant in the past month** (up from 39% in 2024), but discovery is fragmented: 38% word of mouth, 30% in-person discovery, with social media growing for urban diners ([PMQ Pizza/Toast survey](https://www.pmq.com/how-are-guests-discovering-new-restaurants-this-national-survey-has-answers)).

**Gap:** No app currently combines the simplicity of swiping with the personalization of AI filtering in a native mobile experience.

### 2.2 Yelp's Rating System Is Broken

- Yelp ratings cluster almost entirely between **3.5–4.5 stars**, slowing discovery and making differentiation nearly impossible ([HBS AI Institute](https://aiinstitute.hbs.edu/platform-digit/submission/yelp-is-failing-its-customers-rating-systems-and-the-challenge-of-discovery)).
- Yelp's review volume is declining; Google Reviews and TikTok have displaced it as the primary discovery channel for younger demographics ([Reddit discussion](https://www.reddit.com/r/Yelp/comments/1dgk62c/im_disheartened_that_yelp_reviews_have_lost_so_much_market_share/), [Ulad Shauchenka post-mortem](https://www.uladshauchenka.com/p/how-yelp-lost-the-game-to-google)).

**Gap:** Swipe mechanics bypass the rating problem entirely — users express preference through action (swipe), not through the 5-star rating burden.

### 2.3 No Swipe App Has Loyalty Integration

- None of the competitors integrate with existing restaurant loyalty programs (Toast, Owner.com, LoyaltyPlant, etc.).
- The restaurant loyalty app market is projected to reach **$62.83 billion by 2033** ([ActiveMenus](https://activemenus.com/the-63-billion-mobile-app-revolution-why-independent-restaurants-cant-afford-to-compete-without-their-own-branded-app)).

**Gap:** BiteMatch could become a discovery layer on top of existing loyalty ecosystems.

### 2.4 No B2B Revenue Model in Any Swipe App

- Hangrily is free/premium only. Munch is free. Zesty had no monetization.
- Meanwhile, Yelp generates **~96% of revenue from business advertising** ([TradingView/GuruFocus](https://www.tradingview.com/news/gurufocus:537acb08c094b:0-yelp-inc-fundamentally-solid-but-technically-challenged)).
- OpenTable charges restaurants **$0.25/cover on Basic plan or $49/month**, plus **$1/cover for Google referrals** ([OpenTable Plans](https://www.opentable.com/restaurant-solutions/plans), [Reddit](https://www.reddit.com/r/restaurantowners/comments/1rvlbc2/opentable_takes_1_for_every_google_referral)).

**Gap:** No swipe-based discovery app has built a B2B revenue stream (premium listings, featured placement, lead generation).

### 2.5 Retention Is an Afterthought

- **Median app retains just 4% of users at Day 30** ([UXCam, 2026](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)).
- The average mobile app loses **3 out of 4 users within a month** of install ([UXCam, 2026](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)).
- None of the swipe competitors have gamification beyond the swipe itself — no streaks, no challenges, no social leaderboards, no loyalty points.

**Gap:** Swipe apps treat retention as an afterthought. This is the single biggest opportunity.

---

## 3. Monetization Strategies Beyond Subscriptions

### 3.1 Affiliate Commissions

| Program | Commission | Notes |
|---------|-----------|-------|
| **Grubhub** | 5% per new customer sale | [FlexOffers](https://www.flexoffers.com/affiliate-programs/grubhub-affiliate-program), [Grubhub Affiliate](https://www.grubhub.com/eat/affiliate) |
| **LocalFlavor** | 15% on restaurant certificates, 3% on specials | [OneMoreCupOfCoffee](https://onemorecupof-coffee.com/best-restaurant-affiliate-programs) |
| **OpenTable** | Affiliate program available | [OpenTable Affiliate](https://help.opentable.com/s/article/OpenTable-Affiliate-Program-1505261059868?language=en_US) |
| **DoorDash** | $1,000 referral bonus per merchant (B2B) | [DoorDash Merchant Referral](https://merchants.doordash.com/en-us/about/merchant-referral-program) |

**Application to BiteMatch:** When a user swipes right and gets directions, offer a "Order Now" button that links to Grubhub/DoorDash/UberEats with affiliate tracking. Per-order commission (5–15%) on first-time users is significant.

### 3.2 B2B Restaurant Listings

- **Yelp Ads:** Businesses pay for promoted placement in search results. Yelp claims "4x more leads" with ads ([Yelp for Business](https://business.yelp.com)).
- **OpenTable:** $49/month for Core plan, $0.25/cover for Basic. $1/cover for Google referrals ([OpenTable Plans](https://www.opentable.com/restaurant-solutions/plans)).
- **Restaurant directory monetization:** Premium listings, featured placements, sponsored posts are standard directory revenue models ([Directorist](https://directorist.com/blog/monetize-a-restaurant-directory-website)).

**Application to BiteMatch:** Restaurants could pay for:
- **Featured placement** in swipe deck (clearly labeled as "Sponsored")
- **Promoted cuisine cards** (e.g., "Mexican restaurants near you" — sponsored by a Mexican restaurant)
- **New restaurant spotlight** — pay to be featured for first 30 days

### 3.3 Premium Features (Beyond Paywall)

- **Analytics dashboard** for restaurants: "You appeared in 200 swipe sessions this week, matched 45 times."
- **Promo codes:** Restaurants can push limited-time offers to BiteMatch users in their area.
- **Reservation integration:** Take a commission on reservations booked through BiteMatch (like OpenTable's per-cover model).

### 3.4 In-App Advertising (Future)

- 90%+ of apps are free; ad revenue is the dominant model for high-volume apps ([Business of Apps](https://www.businessofapps.com/marketplace/app-marketing/research/app-monetization-models)).
- **Caution:** Hangrily's differentiator is "no ads." Adding ads too early would hurt retention. Defer until post-MVP.

---

## 4. Retention Mechanics

### 4.1 What Works in Restaurant Apps

- **McDonald's Rewards:** Points on purchases, challenges ("order a certain item"), bonus point events ([Trophy](https://trophy.so/blog/food-drink-gamification-examples)).
- **DoorDash gamification:** Challenges, streaks, badges for Dashers (delivery drivers), which drive engagement ([Trophy](https://trophy.so/blog/food-drink-gamification-examples)).
- **Papa Johns gamified loyalty:** Tiered rewards, mini-games, referral campaigns ([LoyaltyPlant](https://loyaltyplant.com/blog/gamification-loyalty-program-restaurant-growth)).
- **Oracle CrowdTwist:** Polls, quizzes, badges, scavenger hunts, challenges, sweepstakes, instant wins ([Oracle PDF](https://www.oracle.com/a/ocom/docs/industries/food-beverage/restaurant-loyalty-gamification.pdf)).

### 4.2 General App Retention Benchmarks

- **Median app: 4% Day-30 retention** ([UXCam](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)).
- **Apps that get users to a core value action in first session see 2–3x better Day-7 retention** ([UXCam](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)).
- **Gamification market projected to reach $92.5B by 2030, 26% CAGR** ([Mordor Intelligence via OpenLoyalty](https://www.openloyalty.io/insider/gamification-increases-engagement)).

### 4.3 Retention Mechanics Swipe Apps Don't Use

None of the competitors (Hangrily, Munch, Grublr, Chicken Tender, Food Flip) implement:
- ✅ **Streak mechanics** (e.g., "7 days of deciding where to eat")
- ✅ **Weekly challenges** (e.g., "Try a new cuisine every day this week")
- ✅ **Social leaderboards** (e.g., "Most adventurous eater in your city")
- ✅ **Push notification triggers** (e.g., "Hungry? Your usual lunch spot is 0.3 miles away")
- ✅ **Loyalty program integration** (earn points when you visit a matched restaurant)
- ✅ **Referral loops** (invite friends, unlock group mode features)

---

## 5. Top 5 High-Impact, Low-Effort Enhancements for BiteMatch MVP

### Enhancement 1: Swipe-to-Reserve with OpenTable/Resy Integration

**Impact:** HIGH | **Effort:** LOW-MEDIUM

**What:** When a match is found, offer a "Reserve Table" button that deep-links to OpenTable or Resy for booking. Track conversion via affiliate links.

**Why:** 
- OpenTable takes **$1/cover from Google referrals** ([Reddit](https://www.reddit.com/r/restaurantowners/comments/1rvlbc2/opentable_takes_1_for_every_google_referral)), proving per-action monetization works.
- OpenTable's affiliate program exists ([OpenTable Affiliate](https://help.opentable.com/s/article/OpenTable-Affiliate-Program-1505261059868?language=en_US)).
- Grubhub pays **5% per new customer** ([FlexOffers](https://www.flexoffers.com/affiliate-programs/grubhub-affiliate-program)).
- This turns a passive swipe into an actionable outcome, increasing perceived value.

**Implementation:** Add a "Reserve" button on the match screen that opens the OpenTable/Resy app via deep link. Track clicks via RevenueCat events. No backend needed — deep links are client-side.

**Source:** [OpenTable Business Model](https://www.elluminatiinc.com/how-opentable-works), [OpenTable Plans](https://www.opentable.com/restaurant-solutions/plans)

---

### Enhancement 2: "Decider Streaks" — Gamified Consistency Loop

**Impact:** HIGH | **Effort:** LOW

**What:** Track consecutive days the user makes a dining decision via BiteMatch. Display streak count on the swipe screen. Offer a "streak freeze" as a RevenueCat IAP.

**Why:**
- Streak mechanics are proven retention drivers (Duolingo, Snapchat).
- The median app retains only **4% at Day 30** ([UXCam](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)) — streaks directly combat this.
- Local storage only (no backend). Simple counter + daily reset logic.
- Hangrily has no streak mechanic. Neither does any competitor.

**Implementation:**
- Store `lastSwipeDate` in AsyncStorage.
- On app open, check if `lastSwipeDate` was yesterday → increment streak.
- If `lastSwipeDate` was today → show current streak.
- If older than yesterday → reset to 0.
- Display as "🔥 7-day streak" on swipe screen.

**Source:** [UXCam retention benchmarks](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it), [OpenLoyalty gamification](https://www.openloyalty.io/insider/gamification-increases-engagement)

---

### Enhancement 3: "This Week's Challenge" — Weekly Content Loop

**Impact:** HIGH | **Effort:** LOW

**What:** Push a weekly challenge to the user: "This week: try a new Italian restaurant," "This week: find a restaurant under $15," "This week: visit a place you've never been to." Users check off completion.

**Why:**
- McDonald's uses challenges to drive repeat orders ([Trophy](https://trophy.so/blog/food-drink-gamification-examples)).
- Challenges create a reason to open the app weekly, even when not hungry.
- 64% of U.S. adults tried a new restaurant in the past month ([PMQ Pizza/Toast](https://www.pmq.com/how-are-guests-discovering-new-restaurants-this-national-survey-has-answers)) — challenges align with this behavior.
- Challenges can be stored locally or pulled from a simple JSON config. No backend required for MVP.

**Implementation:**
- Hardcode 8–12 weekly challenges in a JSON file.
- Rotate based on `Math.floor(Date.now() / (7 * 24 * 60 * 60 * 1000))` (week number).
- Show on a "Challenges" tab. Allow check-off with local storage.
- Push notification: "Your weekly challenge is live 🍕"

**Source:** [PMQ Pizza/Toast survey](https://www.pmq.com/how-are-guests-discovering-new-restaurants-this-national-survey-has-answers), [Trophy gamification examples](https://trophy.so/blog/food-drink-gamification-examples)

---

### Enhancement 4: Smart Push Notifications — "Hungry?" Triggers

**Impact:** HIGH | **Effort:** LOW

**What:** Time-based push notifications that trigger when users are most likely to be deciding where to eat:
- **11:00 AM:** "Lunch decision time. What sounds good?"
- **5:00 PM:** "Dinner plans? Swipe to decide."
- **Location-based (opt-in):** "3 new restaurants opened within 1 mile."

**Why:**
- OLO's research identifies push notifications as a key engagement strategy for restaurant apps ([OLO](https://www.olo.com/blog/strategies-for-maximizing-restaurant-app-engagement)).
- Apps that get users to a core value action in the first session see **2–3x better Day-7 retention** ([UXCam](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)).
- Push notifications bring users back. The swipe mechanic is the value action.
- Expo Push Notifications are built-in. No backend needed for scheduled local notifications.

**Implementation:**
- Use `expo-notifications` for local scheduled notifications.
- Default: 11 AM and 5 PM daily.
- User can disable in settings.
- Track open rate via RevenueCat events.

**Source:** [OLO engagement strategies](https://www.olo.com/blog/strategies-for-maximizing-restaurant-app-engagement), [UXCam retention](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it)

---

### Enhancement 5: "Visit Stamps" — Post-Visit Check-In System

**Impact:** MEDIUM-HIGH | **Effort:** LOW

**What:** After a match, allow the user to "check in" when they visit the restaurant. This earns a "stamp" on a digital passport. Collecting stamps from different restaurants/cuisines unlocks badges.

**Why:**
- The gamification market is growing at **26% CAGR** ([Mordor Intelligence via OpenLoyalty](https://www.openloyalty.io/insider/gamification-increases-engagement)).
- Oracle CrowdTwist uses badges and scavenger hunts for restaurant loyalty ([Oracle PDF](https://www.oracle.com/a/ocom/docs/industries/food-beverage/restaurant-loyalty-gamification.pdf)).
- Creates a closed loop: discover → decide → visit → record → reward.
- Data from check-ins can inform future swipe recommendations (personalization).
- All local storage. No backend.

**Implementation:**
- After match screen: "Visited?" button.
- If yes → add stamp to local `visitedRestaurants` array.
- Display "passport" view showing visited restaurants as stamps.
- Badge logic: "First Italian," "5-restaurant club," "Cuisine explorer."
- Badge data stored in AsyncStorage.

**Source:** [Oracle CrowdTwist gamification](https://www.oracle.com/a/ocom/docs/industries/food-beverage/restaurant-loyalty-gamification.pdf), [Mordor Intelligence gamification](https://www.openloyalty.io/insider/gamification-increases-engagement)

---

## Summary Matrix

| # | Enhancement | Impact | Effort | Revenue Potential | Retention Impact |
|---|------------|--------|--------|-------------------|-----------------|
| 1 | Swipe-to-Reserve (OpenTable/Grubhub deep links) | HIGH | LOW-MED | Affiliate commissions per conversion | Medium (actionable outcome) |
| 2 | Decider Streaks | HIGH | LOW | IAP (streak freeze) | HIGH (daily return loop) |
| 3 | Weekly Challenges | HIGH | LOW | None direct | HIGH (weekly content loop) |
| 4 | Smart Push Notifications | HIGH | LOW | None direct | HIGH (re-engagement) |
| 5 | Visit Stamps / Passport | MED-HIGH | LOW | Future B2B data play | HIGH (closed-loop behavior) |

**All 5 can be implemented within the MVP scope** using local storage, Expo Push Notifications, and deep links. None require a backend. RevenueCat can track engagement events for all of them.

---

## Open Questions / Unverified Claims

1. **Hangrily's exact pricing:** Not publicly listed. REPORTED — need to check their pricing page directly.
2. **Munch app current status:** Last known activity was 2021. UNVERIFIED — may be abandoned.
3. **Grublr active user base:** Only visible via TikTok/social media presence. UNVERIFIED — no download numbers found.
4. **OpenTable affiliate commission rate:** Program exists but specific commission % not publicly disclosed. REPORTED — need to apply to their partner program.
5. **Zesty shutdown reason:** Attributed to feature absorption into core DoorDash app. REPORTED — DoorDash did not confirm failure, framed as "transition."

---

## References

1. [Hangrily Features](https://hangrily.app/features) — Feature overview, pricing model
2. [Hangrily: Tinder for Restaurants](https://hangrily.app/tinder-for-restaurants) — Product positioning
3. [NRN: DoorDash Winds Down Zesty](https://www.nrn.com/restaurant-technology/doordash-winds-down-restaurant-discovery-app-zesty) — Zesty shutdown, April 2026
4. [Restaurant Business Online: Zesty Testing](https://restaurantbusinessonline.com/technology/doordash-testing-restaurant-discovery-app-called-zesty) — Zesty feature details
5. [Bloomberg: DoorDash Launches Zesty](https://www.bloomberg.com/news/articles/2025-12-16/doordash-launches-zesty-an-ai-powered-app-for-finding-new-restaurants) — Zesty launch details
6. [AlleyWatch: Munch](https://www.alleywatch.com/2021/02/munch-group-dining-discovery-app-chris-desantis) — Munch app history
7. [Reputation/Nielsen: AI in Dining](https://reputation.com/resources/press/new-reputation-survey-reveals-how-ai-and-economic-pressures-are-rewriting-the-rules-of-dining-out-in-america) — 55% trust AI reviews, 45% of 35-44 use ChatGPT
8. [PMQ Pizza/Toast: Restaurant Discovery Survey](https://www.pmq.com/how-are-guests-discovering-new-restaurants-this-national-survey-has-answers) — 64% tried new restaurant, 38% word of mouth
9. [NextAtlas: Exhausted Eater](https://blog.nextatlas.com/article/exhausted-eater-how-decision-fatigue-is-reshaping-food-and-beverage-with-future-food-institute) — 114% YoY growth in decision fatigue conversations
10. [HBS AI Institute: Yelp Is Failing](https://aiinstitute.hbs.edu/platform-digit/submission/yelp-is-failing-its-customers-rating-systems-and-the-challenge-of-discovery) — Yelp rating clustering 3.5-4.5
11. [UXCam: App Retention 2026](https://www.appcues.com/blog/app-retention-is-hard-heres-how-to-improve-it) — 4% Day-30 retention, 3/4 users lost within a month
12. [OLO: Restaurant App Engagement](https://www.olo.com/blog/strategies-for-maximizing-restaurant-app-engagement) — Push notifications as key engagement strategy
13. [Trophy: Food & Drink Gamification](https://trophy.so/blog/food-drink-gamification-examples) — McDonald's, DoorDash gamification examples
14. [Oracle: Restaurant Loyalty Gamification](https://www.oracle.com/a/ocom/docs/industries/food-beverage/restaurant-loyalty-gamification.pdf) — CrowdTwist gamification features
15. [OpenLoyalty: Gamification Increases Engagement](https://www.openloyalty.io/insider/gamification-increases-engagement) — $92.5B gamification market by 2030
16. [FlexOffers: Grubhub Affiliate](https://www.flexoffers.com/affiliate-programs/grubhub-affiliate-program) — 5% commission
17. [OneMoreCupOfCoffee: Restaurant Affiliate Programs](https://onemorecupof-coffee.com/best-restaurant-affiliate-programs) — LocalFlavor 15% on certificates
18. [OpenTable Affiliate](https://help.opentable.com/s/article/OpenTable-Affiliate-Program-1505261059868?language=en_US) — Affiliate program exists
19. [OpenTable Plans](https://www.opentable.com/restaurant-solutions/plans) — $49/month, $0.25/cover
20. [Yelp for Business](https://business.yelp.com) — Yelp Ads, 4x more leads claim
21. [Alaina Zemanick: Pickr Portfolio](https://www.alainazemanick.com/portfolio/pickr) — Pickr restaurant app concept
22. [Google Play: Pickr](https://play.google.com/store/apps/details?id=com.future.proof.apps.decider) — General decision maker app
23. [Apple App Store: Chicken Tender](https://apps.apple.com/us/app/chicken-tender/id1514175364) — Bluetooth group sync
24. [Apple App Store: Food Flip](https://apps.apple.com/us/app/food-flip-decide-where-to-eat/id6740217074) — Bracket-style picker
25. [Apple App Store: Food Swiper](https://apps.apple.com/us/app/food-swiper-find-food/id1581852879) — Real-time group sessions
26. [hungree.ca](https://hungree.ca) — Canadian group dining app
27. [TradingView: Yelp Fundamentally Solid](https://www.tradingview.com/news/gurufocus:537acb08c094b:0-yelp-inc-fundamentally-solid-but-technically-challenged) — 96% revenue from business ads
28. [Directorist: Monetize Restaurant Directory](https://directorist.com/blog/monetize-a-restaurant-directory-website) — Premium listings, featured placements
29. [Business of Apps: App Monetization Models](https://www.businessofapps.com/marketplace/app-marketing/research/app-monetization-models) — 90%+ free apps, ad revenue dominant
30. [LoyaltyPlant: 2025 Restaurant Loyalty Review](https://loyaltyplant.com/blog/2025-in-review-2026-ahead-loyaltyplant) — Gamification trends
31. [ActiveMenus: $63B Mobile App Revolution](https://activemenus.com/the-63-billion-mobile-app-revolution-why-independent-restaurants-cant-afford-to-compete-without-their-own-branded-app) — $62.83B by 2033
