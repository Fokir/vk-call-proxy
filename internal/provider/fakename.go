package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"
)

var maleFirstNames = []string{
	"Андрей", "Александр", "Алексей", "Артём", "Антон",
	"Борис", "Вадим", "Валерий", "Виктор", "Владимир",
	"Владислав", "Глеб", "Григорий", "Даниил", "Денис",
	"Дмитрий", "Евгений", "Егор", "Иван", "Игорь",
	"Илья", "Кирилл", "Константин", "Леонид", "Максим",
	"Матвей", "Михаил", "Никита", "Николай", "Олег",
	"Павел", "Пётр", "Роман", "Руслан", "Сергей",
	"Степан", "Тимофей", "Фёдор", "Юрий", "Ярослав",
	"Саша", "Женя", "Лёша", "Дима", "Миша",
	"Коля", "Серёжа",
}

var femaleFirstNames = []string{
	"Анна", "Алина", "Анастасия", "Валерия", "Вероника",
	"Виктория", "Дарья", "Евгения", "Екатерина", "Елена",
	"Ирина", "Кристина", "Ксения", "Мария", "Марина",
	"Наталья", "Ольга", "Полина", "Светлана", "Татьяна",
	"Саша", "Женя", "Вика", "Катя", "Наташа",
	"Алёна", "Юлия", "Диана", "Софья", "Маша",
}

// lastNamePairs stores male and female forms of each surname.
type lastNamePair struct {
	Male   string
	Female string
}

var lastNames = []lastNamePair{
	{"Иванов", "Иванова"}, {"Петров", "Петрова"}, {"Сидоров", "Сидорова"},
	{"Кузнецов", "Кузнецова"}, {"Соколов", "Соколова"}, {"Попов", "Попова"},
	{"Лебедев", "Лебедева"}, {"Козлов", "Козлова"}, {"Новиков", "Новикова"},
	{"Морозов", "Морозова"}, {"Волков", "Волкова"}, {"Алексеев", "Алексеева"},
	{"Семёнов", "Семёнова"}, {"Егоров", "Егорова"}, {"Павлов", "Павлова"},
	{"Степанов", "Степанова"}, {"Николаев", "Николаева"}, {"Орлов", "Орлова"},
	{"Андреев", "Андреева"}, {"Макаров", "Макарова"}, {"Никитин", "Никитина"},
	{"Захаров", "Захарова"}, {"Зайцев", "Зайцева"}, {"Соловьёв", "Соловьёва"},
	{"Борисов", "Борисова"}, {"Яковлев", "Яковлева"}, {"Григорьев", "Григорьева"},
	{"Романов", "Романова"}, {"Воробьёв", "Воробьёва"}, {"Сергеев", "Сергеева"},
	{"Кузьмин", "Кузьмина"}, {"Фролов", "Фролова"}, {"Александров", "Александрова"},
	{"Дмитриев", "Дмитриева"}, {"Королёв", "Королёва"}, {"Гусев", "Гусева"},
	{"Киселёв", "Киселёва"}, {"Ильин", "Ильина"}, {"Максимов", "Максимова"},
	{"Поляков", "Полякова"}, {"Сорокин", "Сорокина"}, {"Виноградов", "Виноградова"},
	{"Ковалёв", "Ковалёва"}, {"Белов", "Белова"}, {"Медведев", "Медведева"},
	{"Антонов", "Антонова"}, {"Тарасов", "Тарасова"}, {"Жуков", "Жукова"},
}

var nickPrefixes = []string{
	"cool", "dark", "pro", "mega", "neo",
	"mr", "just", "real", "big", "old",
	"ice", "mad", "top", "fast", "true",
	"super", "best", "the", "epic", "wild",
}

var nickBases = []string{
	"wolf", "hawk", "fox", "bear", "lion",
	"tiger", "eagle", "shark", "viper", "cobra",
	"phantom", "shadow", "ghost", "storm", "blaze",
	"frost", "thunder", "ninja", "sniper", "hunter",
	"gamer", "coder", "hacker", "pilot", "racer",
	"knight", "king", "chief", "boss", "legend",
}

var translitMap = map[rune]string{
	'А': "A", 'Б': "B", 'В': "V", 'Г': "G", 'Д': "D",
	'Е': "E", 'Ё': "E", 'Ж': "Zh", 'З': "Z", 'И': "I",
	'Й': "Y", 'К': "K", 'Л': "L", 'М': "M", 'Н': "N",
	'О': "O", 'П': "P", 'Р': "R", 'С': "S", 'Т': "T",
	'У': "U", 'Ф': "F", 'Х': "Kh", 'Ц': "Ts", 'Ч': "Ch",
	'Ш': "Sh", 'Щ': "Shch", 'Ъ': "", 'Ы': "Y", 'Ь': "",
	'Э': "E", 'Ю': "Yu", 'Я': "Ya",
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d",
	'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i",
	'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n",
	'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t",
	'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch",
	'ш': "sh", 'щ': "shch", 'ъ': "", 'ы': "y", 'ь': "",
	'э': "e", 'ю': "yu", 'я': "ya",
}

func transliterate(s string) string {
	var b strings.Builder
	for _, r := range s {
		if lat, ok := translitMap[r]; ok {
			b.WriteString(lat)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// randomGender returns true for male, false for female.
func randomGender() bool {
	return rand.IntN(2) == 0
}

func randomFirstName(male bool) string {
	if male {
		return maleFirstNames[rand.IntN(len(maleFirstNames))]
	}
	return femaleFirstNames[rand.IntN(len(femaleFirstNames))]
}

func randomLastName(male bool) string {
	pair := lastNames[rand.IntN(len(lastNames))]
	if male {
		return pair.Male
	}
	return pair.Female
}

// RandomDisplayName generates a random display name for VK conference.
//
// Distribution:
//
//	25% — Russian first name only
//	25% — Russian last name only (gender-matched form)
//	25% — First + Last or Last + First (gender-matched, 50/50 order)
//	25% — Latin nickname
func RandomDisplayName() string {
	male := randomGender()
	switch rand.IntN(4) {
	case 0:
		return randomFirstName(male)
	case 1:
		return randomLastName(male)
	case 2:
		first := randomFirstName(male)
		last := randomLastName(male)
		if rand.IntN(2) == 0 {
			return first + " " + last
		}
		return last + " " + first
	default:
		return randomNickname()
	}
}

// DeriveDisplayNames generates deterministic display names from a shared token.
// Both client and server independently compute the same name pairs.
// Returns serverNames[0..n-1] and clientNames[0..n-1].
// Names look like natural Telemost participant names (Russian names, nicknames).
func DeriveDisplayNames(token string, n int) (serverNames, clientNames []string) {
	used := make(map[string]bool)
	serverNames = make([]string, n)
	clientNames = make([]string, n)

	for i := 0; i < n; i++ {
		serverNames[i] = deriveUniqueName(token, "s", i, used)
	}
	for i := 0; i < n; i++ {
		clientNames[i] = deriveUniqueName(token, "c", i, used)
	}
	return
}

// deriveUniqueName generates a deterministic name and retries with incrementing
// suffix if the name collides with an already-used one.
func deriveUniqueName(token, role string, index int, used map[string]bool) string {
	for attempt := 0; attempt < 100; attempt++ {
		name := deriveName(token, role, index+attempt*1000)
		if !used[name] {
			used[name] = true
			return name
		}
	}
	// Fallback: should never happen with reasonable name space.
	name := deriveName(token, role, index)
	used[name] = true
	return name
}

// deriveName produces a single deterministic display name from HMAC(token, role+index).
func deriveName(token, role string, index int) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(fmt.Sprintf("%s-%d", role, index)))
	h := mac.Sum(nil) // 32 bytes

	// Use first 8 bytes as deterministic seed.
	seed := binary.BigEndian.Uint64(h[:8])
	r := rand.New(rand.NewPCG(seed, binary.BigEndian.Uint64(h[8:16])))

	male := r.IntN(2) == 0
	switch r.IntN(4) {
	case 0:
		// First name only.
		return seededFirstName(r, male)
	case 1:
		// Last name only.
		return seededLastName(r, male)
	case 2:
		// First + Last or Last + First.
		first := seededFirstName(r, male)
		last := seededLastName(r, male)
		if r.IntN(2) == 0 {
			return first + " " + last
		}
		return last + " " + first
	default:
		// Latin nickname.
		return seededNickname(r)
	}
}

func seededFirstName(r *rand.Rand, male bool) string {
	if male {
		return maleFirstNames[r.IntN(len(maleFirstNames))]
	}
	return femaleFirstNames[r.IntN(len(femaleFirstNames))]
}

func seededLastName(r *rand.Rand, male bool) string {
	pair := lastNames[r.IntN(len(lastNames))]
	if male {
		return pair.Male
	}
	return pair.Female
}

func seededNickname(r *rand.Rand) string {
	switch r.IntN(3) {
	case 0:
		pair := lastNames[r.IntN(len(lastNames))]
		base := strings.ToLower(transliterate(pair.Male))
		return fmt.Sprintf("%s%d", base, r.IntN(100))
	case 1:
		return nickPrefixes[r.IntN(len(nickPrefixes))] + nickBases[r.IntN(len(nickBases))]
	default:
		return fmt.Sprintf("%s%d", nickBases[r.IntN(len(nickBases))], r.IntN(1000))
	}
}

func randomNickname() string {
	switch rand.IntN(3) {
	case 0:
		// transliterated surname + digits
		pair := lastNames[rand.IntN(len(lastNames))]
		base := strings.ToLower(transliterate(pair.Male))
		return fmt.Sprintf("%s%d", base, rand.IntN(100))
	case 1:
		// prefix + base
		return nickPrefixes[rand.IntN(len(nickPrefixes))] + nickBases[rand.IntN(len(nickBases))]
	default:
		// base + digits
		return fmt.Sprintf("%s%d", nickBases[rand.IntN(len(nickBases))], rand.IntN(1000))
	}
}
