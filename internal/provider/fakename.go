package provider

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// MaleFirstNames is the pool of male first names used for display name generation.
var MaleFirstNames = []string{
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

// FemaleFirstNames is the pool of female first names used for display name generation.
var FemaleFirstNames = []string{
	"Анна", "Алина", "Анастасия", "Валерия", "Вероника",
	"Виктория", "Дарья", "Евгения", "Екатерина", "Елена",
	"Ирина", "Кристина", "Ксения", "Мария", "Марина",
	"Наталья", "Ольга", "Полина", "Светлана", "Татьяна",
	"Саша", "Женя", "Вика", "Катя", "Наташа",
	"Алёна", "Юлия", "Диана", "Софья", "Маша",
}

// LastNamePair stores male and female forms of each surname.
type LastNamePair struct {
	Male   string
	Female string
}

// LastNames is the pool of surname pairs used for display name generation.
var LastNames = []LastNamePair{
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

// NickPrefixes is the pool of nickname prefixes used for display name generation.
var NickPrefixes = []string{
	"cool", "dark", "pro", "mega", "neo",
	"mr", "just", "real", "big", "old",
	"ice", "mad", "top", "fast", "true",
	"super", "best", "the", "epic", "wild",
}

// NickBases is the pool of nickname base words used for display name generation.
var NickBases = []string{
	"wolf", "hawk", "fox", "bear", "lion",
	"tiger", "eagle", "shark", "viper", "cobra",
	"phantom", "shadow", "ghost", "storm", "blaze",
	"frost", "thunder", "ninja", "sniper", "hunter",
	"gamer", "coder", "hacker", "pilot", "racer",
	"knight", "king", "chief", "boss", "legend",
}

// TranslitMap maps Cyrillic runes to their Latin transliteration.
var TranslitMap = map[rune]string{
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

// Transliterate converts a Cyrillic string to Latin using TranslitMap.
func Transliterate(s string) string {
	var b strings.Builder
	for _, r := range s {
		if lat, ok := TranslitMap[r]; ok {
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
		return MaleFirstNames[rand.IntN(len(MaleFirstNames))]
	}
	return FemaleFirstNames[rand.IntN(len(FemaleFirstNames))]
}

func randomLastName(male bool) string {
	pair := LastNames[rand.IntN(len(LastNames))]
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

func randomNickname() string {
	switch rand.IntN(3) {
	case 0:
		// transliterated surname + digits
		pair := LastNames[rand.IntN(len(LastNames))]
		base := strings.ToLower(Transliterate(pair.Male))
		return fmt.Sprintf("%s%d", base, rand.IntN(100))
	case 1:
		// prefix + base
		return NickPrefixes[rand.IntN(len(NickPrefixes))] + NickBases[rand.IntN(len(NickBases))]
	default:
		// base + digits
		return fmt.Sprintf("%s%d", NickBases[rand.IntN(len(NickBases))], rand.IntN(1000))
	}
}
